'use strict';
const {readdirSync} = require('node:fs');
const path = require('node:path');
const {spawnSync} = require('node:child_process');

const playwright = require.resolve(process.env.PLAYWRIGHT_MODULE || 'playwright');
const tests = readdirSync(__dirname).filter(name => name.endsWith('_test.cjs')).sort().map(name => path.join(__dirname, name));
const result = spawnSync(process.execPath, ['--test', ...tests], {
  cwd: path.resolve(__dirname, '..'),
  stdio: 'inherit',
  env: {...process.env, PLAYWRIGHT_MODULE: playwright},
});
if (result.error) throw result.error;
process.exitCode = result.status ?? 1;
