import { readdirSync, readFileSync } from 'node:fs';
import { dirname, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, test } from 'vitest';
import ts from 'typescript';
import { parse } from 'vue/compiler-sfc';

const root = dirname(fileURLToPath(import.meta.url));
const normalize = (path: string) => path.replaceAll('\\', '/');
function sources(directory = root): string[] {
  return readdirSync(directory, {withFileTypes:true}).flatMap(entry => {
    const path = resolve(directory, entry.name);
    return entry.isDirectory() ? sources(path) : /\.(?:ts|vue)$/.test(entry.name) && !entry.name.endsWith('.test.ts') ? [path] : [];
  });
}
function importPath(node: ts.Node): string | undefined {
  if ((ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) && node.moduleSpecifier && ts.isStringLiteral(node.moduleSpecifier)) return node.moduleSpecifier.text;
  if (ts.isCallExpression(node) && (node.expression.kind === ts.SyntaxKind.ImportKeyword || ts.isIdentifier(node.expression) && node.expression.text === 'require')) {
    const argument = node.arguments[0];
    if (argument && ts.isStringLiteral(argument)) return argument.text;
  }
  if (ts.isImportTypeNode(node) && ts.isLiteralTypeNode(node.argument) && ts.isStringLiteral(node.argument.literal)) return node.argument.literal.text;
}
function localImport(file: string, specifier: string): string | undefined {
  const path = specifier.startsWith('@/') ? resolve(root, specifier.slice(2)) : specifier.startsWith('.') ? resolve(dirname(file), specifier) : undefined;
  return path && normalize(relative(root, path));
}
const domIdentifiers = new Set(['window', 'document', 'localStorage', 'sessionStorage', 'navigator', 'Document', 'Window', 'HTMLElement', 'SVGElement', 'Element', 'Node']);

test('HTTP and workspace models depend only on their own layer and lower-level contracts', () => {
  const violations: string[] = [];
  for (const file of sources()) {
    const name = normalize(relative(root, file));
    const api = name.startsWith('api/'), model = name.startsWith('workspace/model/');
    if (!api && !model) continue;
    const source = ts.createSourceFile(file, readFileSync(file, 'utf8'), ts.ScriptTarget.Latest, true);
    const visit = (node: ts.Node) => {
      const specifier = importPath(node);
      if (specifier) {
        const destination = localImport(file, specifier);
        if (/^(?:vue(?:\/|$|-)|@vue\/)/.test(specifier) || destination && !(destination.startsWith('api/') || model && destination.startsWith('workspace/model/'))) {
          violations.push(`${name}: reverse dependency on ${specifier}`);
        }
      }
      if (ts.isIdentifier(node) && domIdentifiers.has(node.text)) violations.push(`${name}: DOM dependency ${node.text}`);
      ts.forEachChild(node, visit);
    };
    visit(source);
  }
  expect(violations).toEqual([]);
});

test('production modules use typed imports and Vue templates instead of legacy globals or HTML renderers', () => {
  const violations: string[] = [];
  for (const file of sources()) {
    const name = normalize(relative(root, file)), contents = readFileSync(file, 'utf8');
    let scripts = [contents];
    if (file.endsWith('.vue')) {
      const {descriptor, errors} = parse(contents, {filename:file});
      expect(errors, name).toEqual([]);
      scripts = [descriptor.script?.content, descriptor.scriptSetup?.content].filter((value): value is string => value !== undefined);
      const inspectTemplate = (value: unknown): void => {
        if (Array.isArray(value)) { value.forEach(inspectTemplate); return; }
        if (!value || typeof value !== 'object') return;
        const node = value as Record<string, unknown>;
        if (node.type === 7 && node.name === 'html') violations.push(`${name}: v-html bypasses escaped templates`);
        for (const key of ['children', 'props', 'branches']) inspectTemplate(node[key]);
      };
      inspectTemplate(descriptor.template?.ast);
    }
    for (const script of scripts) {
      const scanner = ts.createScanner(ts.ScriptTarget.Latest, false, ts.LanguageVariant.Standard, script);
      for (let token = scanner.scan(); token !== ts.SyntaxKind.EndOfFileToken; token = scanner.scan()) {
        if ((token === ts.SyntaxKind.SingleLineCommentTrivia || token === ts.SyntaxKind.MultiLineCommentTrivia) && /@ts-(?:nocheck|ignore)\b/.test(scanner.getTokenText())) violations.push(`${name}: TypeScript suppression`);
      }
      const source = ts.createSourceFile(file, script, ts.ScriptTarget.Latest, true);
      const visit = (node: ts.Node) => {
        if (node.kind === ts.SyntaxKind.AnyKeyword) violations.push(`${name}: untyped any`);
        if (ts.isIdentifier(node) && /^PwnMesh[A-Z]/.test(node.text)) violations.push(`${name}: legacy browser global ${node.text}`);
        const specifier = importPath(node), destination = specifier && localImport(file, specifier);
        if (destination?.startsWith('../static/')) violations.push(`${name}: legacy static module import`);
        if (ts.isPropertyAccessExpression(node) && ['innerHTML', 'outerHTML', 'insertAdjacentHTML'].includes(node.name.text)) violations.push(`${name}: imperative HTML renderer ${node.name.text}`);
        if (ts.isElementAccessExpression(node) && ts.isStringLiteral(node.argumentExpression)) {
          const key = node.argumentExpression.text;
          if (/^PwnMesh[A-Z]/.test(key) || ['innerHTML', 'outerHTML', 'insertAdjacentHTML'].includes(key)) violations.push(`${name}: legacy renderer/global ${key}`);
        }
        ts.forEachChild(node, visit);
      };
      visit(source);
    }
  }
  expect(violations).toEqual([]);
});
