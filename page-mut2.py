import subprocess,os
os.chdir('/private/tmp/claude-501/-Users-dayna-code/a20c4422-caf8-4114-92dd-7b8fad7f7ecf/scratchpad/wt-editor3/flowedit')
env=dict(os.environ)
env.update({'BIDE_EDIT_BROWSER':'1','CHROME_BIN':os.path.expanduser('~/.cache/puppeteer/chrome/mac_arm-152.0.7977.75/chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing'),'PATH':'/opt/homebrew/bin:'+env['PATH'],'GOCACHE':'/private/tmp/claude-501/-Users-dayna-code/a20c4422-caf8-4114-92dd-7b8fad7f7ecf/scratchpad/gocache-p3'})
env.pop('GOWORK',None)
tests='TestStackedPortsInChrome|TestDenseGraphPortInChrome|TestUncertifiedSaveDialogInChrome|TestTypedModeInChrome'
muts=[
 ("  l = Math.min(l, x - R);\n  r = Math.max(r, x + R);\n  t = Math.min(t, y - R);\n  b = Math.max(b, y + R);",""),
 ("    if (ox <= oy) {","    if (true) {"),
 ("    if (ox <= 0 || oy <= 0) continue;","    if (ox <= 0 && oy <= 0) continue;"),
 ("  if (w.not_certified) kinds.push(['cannot be certified', 'saving without the full check']);","  if (false) kinds.push(['cannot be certified', 'saving without the full check']);"),
 ("  if ((w.weakened || []).length) kinds.push(['weakens the policy', 'weakening this policy']);","  if (true) kinds.push(['weakens the policy', 'weakening this policy']);"),
 ("n.name + ' \\u2192 ' + w.to + differs(n, w.to, v)","n.name + ' \\u2192 ' + w.to"),
]
for a,b in muts:
    f='page/editor.js'
    s=open(f).read(); assert a in s,a[:40]
    open(f,'w').write(s.replace(a,b,1))
    r=subprocess.run(['go','test','-count=1','-run',tests,'.'],capture_output=True,env=env)
    print('SURVIVED' if r.returncode==0 else 'killed',a[:50].replace('\n',' '),flush=True)
    open(f,'w').write(s)
print('done',flush=True)
