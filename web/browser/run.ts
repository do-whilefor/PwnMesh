import {readdirSync} from 'node:fs';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import {createRequire} from 'node:module';
import {fileURLToPath} from 'node:url';

const directory = path.dirname(fileURLToPath(import.meta.url));
export function discoverBrowserTests(sourceDirectory: string, outputDirectory: string, filters: string[] = []): string[] {
  return readdirSync(sourceDirectory)
    .filter(name => name.endsWith('_test.ts') && (!filters.length || filters.some(filter => name.includes(filter))))
    .sort().map(name => path.join(outputDirectory, name.replace(/\.ts$/, '.js')));
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const require = createRequire(import.meta.url);
  const playwright = require.resolve(process.env.PLAYWRIGHT_MODULE || 'playwright');
  const filters = process.argv.slice(2);
  const tests = discoverBrowserTests(path.resolve(directory, '../../browser'), directory, filters);
  if (!tests.length) throw new Error('No browser suites match: ' + filters.join(', '));
  const result = spawnSync(process.execPath, ['--test', ...tests], {
    cwd: path.resolve(directory, '../..'),
    stdio: 'inherit',
    env: {...process.env, PLAYWRIGHT_MODULE: playwright},
  });
  if (result.error) throw result.error;
  process.exitCode = result.status ?? 1;
}
