import re,sys
for f in sys.argv[1:]:
    s=open(f).read()
    s=s.replace("readConfig(s.path, s.id)","readConfig(s.path)").replace("readConfig(s.root, s.base, s.path, s.id)","readConfig(s.root, s.base, s.path)")
    s=re.sub(r'\t// The file is a new one now[^\n]*\n\tif st, serr := [^\n]*\n\t\ts.id = st\n\t}\n','',s)
    s=re.sub(r'\tid +os.FileInfo[^\n]*\n','',s)
    open(f,'w').write(s)
