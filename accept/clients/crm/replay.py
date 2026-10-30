import re,sys,subprocess,os,psycopg2
log=subprocess.run(["docker","logs","crm-pg"],capture_output=True,text=True).stderr
ents=[];cur=None
hdr=re.compile(r'^\d{4}-\d\d-\d\d \S+ \S+ \[\d+\] (LOG|DETAIL|ERROR|STATEMENT):  (.*)$')
for ln in log.split("\n"):
    m=hdr.match(ln)
    if m:
        if cur: ents.append(cur)
        cur=[m.group(1),m.group(2)]
    elif cur is not None and ln.startswith("\t"): cur[1]+="\n"+ln[1:]
if cur: ents.append(cur)
stm=[];i=0
while i<len(ents):
    k,t=ents[i]
    m=re.match(r'(statement|execute [^:]*): (.*)$',t,re.S)
    if k=="LOG" and m:
        sql=m.group(2)
        hasp = i+1<len(ents) and ents[i+1][0]=="DETAIL" and ents[i+1][1].startswith("parameters")
        if hasp:
            vals=dict((int(a),None if b=="NULL" else b[1:-1].replace("''","'")) for a,b in re.findall(r"\$(\d+) = ('(?:[^']|'')*'|NULL)",ents[i+1][1]))
            def sub(mm):
                v=vals.get(int(mm.group(1)))
                return "NULL" if v is None else "'"+v.replace("'","''")+"'"
            sql=re.sub(r"\$(\d+)",sub,sql)
        stm.append(sql)
    i+=1
stm=[s for s in stm if not re.match(r'\s*(select\s+(pg_|\d|current|version)|show|set|begin|commit|rollback)',s,re.I)]
print(len(stm),"statements",file=sys.stderr)
def conn():
    c=psycopg2.connect(host="127.0.0.1",port=int(sys.argv[1]),user=os.environ["CRM_DB_USER"],password=os.environ["CRM_DB_PASSWORD"],dbname="crm",sslmode="disable")
    c.autocommit=True
    return c
c=conn();cur=c.cursor();fails=0
for s in stm:
    try: cur.execute(s)
    except Exception as e:
        fails+=1
        if c.closed:
            c=conn()
        cur=c.cursor()
        print("FAIL",str(e).strip().split("\n")[0][:200],"|||",re.sub(r"\s+"," ",s)[:300])
print("failures",fails,file=sys.stderr)
