'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const root = path.join(__dirname,'static');
// Accepted reference: tmp/pwn-web-demo/v5. Keep the shipped visual assets intact;
// production data and API adapters are deliberately outside this manifest.
const accepted = {
  "style.css": "e1d8264039a608ed891db19010dcbee4d32b901dca8987885a0b78cac1a25891",
  "llm.css": "d4133924a1950253ac498be990f36dc3ff6cc74673a690329c5f2bf7e328db04",
  "shell.css": "1958b368426087ba16bd7ef7938cb55b30e923f0995cea03f51cd597dc22e04e",
  "blackboard.css": "fc7d88685b4e2462c47c71bd144d66902f57aab588a6f9b7bdb4b37263b23c4e",
  "forms.css": "81c75f23362d42389f8cf1fc29c5053a6b1329b8657c9f6b47169c7e884dbeab",
  "inspector.css": "b0447e4c09b8716d4e4f14dca2d2a86ad0308a218238dbb9c8787af649ab46a8",
  "brand.png": "c6da7cfcca10cf191f08257b7c5029635377adb25bbb8ad11d0c51bd5836cbe8",
  "geist-variable.woff2": "a369fcf5628ea2aa4e1b9e2ec6a5b3624e365bda588e1f0f2f12b564f728fbb8",
  "LICENSE-Geist.txt": "930853ee1daa68554d9e35c8a9175affb74f699fad9a5da6ee5ebe76379d9137",
  "assets/LICENSE-lobe-icons.txt": "add9d7531d1b21646317a8958e38fc727506fa39d24bdecb44154d943c82753a",
  "assets/provider-deepseek.svg": "deba5f98a5c1796e20fcac3149bcd7eb8a32f0bdd04d048819400b1f28bd1439",
  "assets/provider-glm.svg": "8174e65ff71647d5c705c920c953a82bd723d9e3e42887bdd4ffc9dfe51207a0",
  "assets/provider-kimi.svg": "8d4f25bdf458cf671667aa875067ab21f5e146b0818fabdc7d493ede997d7d59",
  "assets/provider-sources.json": "b803a052797fd3c4460b1d756e4e52a9fd5c734bcf4717edf9657a6a2cd722a1",
  "graph.css": "b23c2341b9c8726483f756d653ebee2c940b63f18641ec9d457a07f87f4667ba",
  "graph.js": "14aa2ae4b114be790f559737759673d18a96e80b3d6fc7318656d462bccb5ee1",
  "graph-view.js": "f5e44f34e283a8935d022606dc159c10ee8931aaa597a87a690ed67cdabd14c5",
  "graph-data.js": "60bf2c8337bcecaf3f44c9042b861f56ddab0dca6d46a81aee8cddc30a8966db",
  "canvas.js": "43aa965bcb8dfa51700d99887d5ee0323a88c948a8764c099babc83e186f02ce",
  "layout.js": "7e0c284868078c5219aafe5e11442fcc9c9feeea744dcd04d8935515e2f754c4",
  "routing.js": "fba873ccda7cbe3da981f9a6c995e48b937ff70520c74641caaae599653d3725",
  "lucide.min.js": "ab85225ecc1daa2033607856f03dd2dabcbc08e2d260a5b97e17c690aa929766"
};

test('visual assets and graph engine match the accepted V5 demo exactly', () => {
  for (const [name,digest] of Object.entries(accepted)) {
    assert.equal(crypto.createHash('sha256').update(/\.(png|woff2)$/.test(name) ? fs.readFileSync(path.join(root,name)) : fs.readFileSync(path.join(root,name),'utf8').replace(/\r\n/g,'\n')).digest('hex'),digest,name+' drifted from the accepted V5 source');
  }
  assert.match(fs.readFileSync(path.join(root,'LICENSE-lucide.txt'),'utf8'),/ISC License/);
});

test('the real workbench loads the complete V5 cascade in order without the old theme', () => {
  const html = fs.readFileSync(path.join(root,'index.html'),'utf8');
  const sheets = [...html.matchAll(/<link[^>]*rel="stylesheet"[^>]*href="([^"]+)"/g)].map(match=>match[1].split('/').at(-1));
  assert.deepEqual(sheets,['graph.css','style.css','llm.css','shell.css','blackboard.css','forms.css','inspector.css']);
  assert.ok(html.includes('src="/static/lucide.min.js"'));
  assert.doesNotMatch(html,/workbench\.css|models\.css|graph-demo\.js/);
});
