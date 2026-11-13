import fs from 'node:fs'
import path from 'node:path'
import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'
import {execFileSync} from 'node:child_process'
import {fileURLToPath} from 'node:url'

const root=fileURLToPath(new URL('../',import.meta.url))
assert.equal(process.env.GOWORK,'off','qualification requires GOWORK=off')
assert.ok((process.env.GOFLAGS??'').split(/\s+/).every(flag=>!flag||flag==='-mod=readonly'),'source qualification rejects Go flag substitution')
const mod=JSON.parse(execFileSync('go',['mod','edit','-json'],{cwd:root,encoding:'utf8'}))
assert.equal(mod.Module.Path,'github.com/MalenkiySolovey/solovey-component-fallback-html')
assert.ok(!(mod.Replace??[]).length,'source module replacements are forbidden')
assert.deepEqual(Object.fromEntries((mod.Require??[]).map(item=>[item.Path,item.Version])),{'github.com/MalenkiySolovey/solovey-host':'v0.4.0','golang.org/x/net':'v0.62.0'},'unreviewed source module requirements')
const source=JSON.parse(fs.readFileSync(path.join(root,'SOURCE_PROVENANCE.json')))
assert.equal(source.component,'fallback-html')
assert.equal(source.originalCommit,'04bb0c72a779d89b1763885743ac048db8dd1241')
assert.equal(createHash('sha256').update(fs.readFileSync(path.join(root,'LICENSE'))).digest('hex'),source.licenseSHA256,'original license changed')
execFileSync('git',['merge-base','--is-ancestor',source.filteredHistoryCommit,'HEAD'],{cwd:root,stdio:'pipe'})
for(const record of source.files) {
 assert.ok(record.destination&&!record.destination.includes('..')&&!path.isAbsolute(record.destination))
 assert.equal(createHash('sha256').update(fs.readFileSync(path.join(root,record.destination))).digest('hex'),record.destinationSHA256,`mapped source drift: ${record.destination}`)
}
for(const [name,expected] of Object.entries(source.authoredFiles??{}))assert.equal(createHash('sha256').update(fs.readFileSync(path.join(root,name))).digest('hex'),expected,`authored source drift: ${name}`)
const output=execFileSync('go',['list','-deps','-test','-json','./...'],{cwd:root,encoding:'utf8',maxBuffer:16*1024*1024})
const packages=(output.match(/^{[\s\S]*?^}/gm)??[]).map(JSON.parse)
assert.ok(packages.length>0)
const allowed=new Set(['github.com/MalenkiySolovey/solovey-component-fallback-html','github.com/MalenkiySolovey/solovey-host','golang.org/x/net'])
for(const pkg of packages)if(pkg.Module){assert.ok(allowed.has(pkg.Module.Path),`unapproved source dependency ${pkg.ImportPath}`);assert.ok(!pkg.Module.Replace,'source module substitution')}
const walk=directory=>fs.readdirSync(directory,{withFileTypes:true}).flatMap(entry=>entry.name==='node_modules'||entry.name.startsWith('.')?[]:entry.isDirectory()?walk(path.join(directory,entry.name)):[path.join(directory,entry.name)])
for(const filename of walk(root).filter(name=>/\.(go|ts|vue)$/.test(name))) {
 const text=fs.readFileSync(filename,'utf8')
 if(filename.endsWith('.go')&&!filename.endsWith('_test.go')) {
  assert.ok(!new RegExp('"(?:os(?:/[^"]*)?|syscall|unsafe|database/[^"]*|io/fs|gorm[.]io/[^"]*)"').test(text),`Backend authority import in ${filename}`)
  assert.ok(!/\b(?:net\.(?:Listen|ListenPacket|Dial|DialTimeout)|http\.(?:ListenAndServe|Serve|DefaultClient|DefaultTransport|Get|Post))\b/.test(text),`unowned listener or outbound client in ${filename}`)
 }
 assert.ok(!text.includes('github.com/MalenkiySolovey/solovey-ui/'),`Panel dependency ${filename}`)
 assert.ok(!/from\s+['"]@\//.test(text),`private UI import ${filename}`)
 assert.ok(!/\b(?:M[026][A-C]?|R7|Wave|Phase|Goal)[-_]\w+/.test(text),`development-stage identifier ${filename}`)
}
const tracked=execFileSync('git',['ls-files','-z'],{cwd:root,encoding:'utf8'}).split('\0').filter(Boolean)
assert.ok(!tracked.some(name=>name.includes('node_modules/')||name.startsWith('.frontend-check/')),'generated frontend output included')
const descriptors=JSON.parse(fs.readFileSync(path.join(root,'component.json')))
assert.equal(descriptors.id,'fallback-html')
assert.equal(descriptors.delivery,'in-process')
assert.equal(descriptors.defaultEnabled,false)
assert.equal(descriptors.database.tables.length,15)
console.log(JSON.stringify({status:'PASS',packages:packages.length,allowedModules:[...allowed],mappedSources:source.files.length,privatePanelImports:0,filteredHistoryPreserved:true}))
