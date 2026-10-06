import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import ts from 'typescript';
import {sitemapPages} from './sitemap-pages.mjs';
import {websiteFiles} from './website-files.mjs';
const cyr=/[А-Яа-яЁё]/;
const visible=html=>html.replace(/<!--[\s\S]*?-->/g,'').replace(/<(script|style)\b[\s\S]*?<\/\1>/gi,'').replace(/<[^>]+>/g,'').replaceAll('Русский','');
const english=sitemapPages().filter(p=>p.pathname.startsWith('/en/'));
assert.ok(english.length>=14,'English website is incomplete');
for(const page of english){
 const html=readFileSync(page.local,'utf8');
 assert.match(html,/<html lang="en">/);
 const canonical=[...html.matchAll(/<link\b([^>]+)>/g)].find(m=>/rel="canonical"/.test(m[1]));
 assert.ok(canonical?.[1].includes(`href="${page.url}"`),`Missing canonical: ${page.pathname}`);
 for(const lang of ['en','ru','x-default'])assert.ok(html.includes(`hreflang="${lang}"`),`Missing alternate ${lang}: ${page.pathname}`);
 assert.ok(!cyr.test(visible(html)),`Russian UI in ${page.pathname}`);
}
const native=readFileSync('internal/web/setup_static/index.en.html','utf8');assert.ok(!cyr.test(visible(native)),'Russian UI in English setup HTML');
const source=readFileSync('internal/web/setup_static/setup.en.js','utf8'),sf=ts.createSourceFile('setup.en.js',source,ts.ScriptTarget.Latest,true,ts.ScriptKind.JS);
let technical=0;
function visit(n){
 if((ts.isStringLiteral(n)||ts.isNoSubstitutionTemplateLiteral(n)||ts.isTemplateHead(n)||ts.isTemplateMiddle(n)||ts.isTemplateTail(n))&&cyr.test(n.text.replace(/<!--[\s\S]*?-->/g,''))){
  const p=n.parent;
  const lookup=ts.isCallExpression(p)&&ts.isPropertyAccessExpression(p.expression)&&['includes','startsWith','endsWith','replace','match','split'].includes(p.expression.name.text);
  const comparison=ts.isBinaryExpression(p)&&p.operatorToken.kind!==ts.SyntaxKind.PlusToken;
  assert.ok(lookup||comparison,`Russian UI in English setup JavaScript at line ${sf.getLineAndCharacterOfPosition(n.getStart(sf)).line+1}`);technical++;
 }
 ts.forEachChild(n,visit);
}visit(sf);
assert.equal(JSON.parse(readFileSync('apk/public/manifest.en.webmanifest','utf8')).lang,'en');
const swift=readFileSync('desktop/macos/main.swift','utf8');
let nativeLabels=0;
for(const match of swift.matchAll(/"(?:\\.|[^"\\\n])*"/g)){
 if(!cyr.test(match[0]))continue;
 assert.ok(swift.slice(Math.max(0,match.index-10),match.index).endsWith('localized('),'Untranslated macOS UI label');
 const english=swift.slice(match.index+match[0].length).match(/^,\s*("(?:\\.|[^"\\\n])*")/);
 assert.ok(english&&!cyr.test(english[1])&&english[1].length>2,'Missing English macOS label');nativeLabels++;
}
assert.ok(nativeLabels>=24,'macOS labels are incomplete');
const installer=readFileSync('installer/remotai.iss','utf8');
const installerMessages={russian:new Map(),english:new Map()};
let installerSection='';
for(const rawLine of installer.split(/\r?\n/)){
 const line=rawLine.trim();
 if(!line||line.startsWith(';'))continue;
 if(line.startsWith('[')){installerSection=line;continue;}
 if(installerSection==='[CustomMessages]'){
  const message=line.match(/^(russian|english)\.([^=]+)=(.*)$/);
  assert.ok(message,`Unscoped installer message: ${line}`);
  const [,language,key,value]=message;
  assert.ok(value&&!installerMessages[language].has(key),`Invalid installer message: ${language}.${key}`);
  installerMessages[language].set(key,value);
  if(language==='english')assert.ok(!cyr.test(value),`Russian installer message: ${key}`);
 }
 if(['[Tasks]','[Icons]','[Run]'].includes(installerSection))assert.ok(!cyr.test(line),'Untranslated Windows installer label');
}
assert.deepEqual([...installerMessages.english.keys()].sort(),[...installerMessages.russian.keys()].sort(),'Installer languages have different messages');
for(const [key,value] of installerMessages.english){
 const parameters=text=>[...text.matchAll(/%(?:n|[0-9]+)/g)].map(match=>match[0]).sort();
 assert.deepEqual(parameters(value),parameters(installerMessages.russian.get(key)),`Installer placeholders differ: ${key}`);
}
console.log(`Language surfaces passed: ${english.length} English pages, ${websiteFiles().length} published files, native wizard, PWA (${technical} server error lookups)`);
