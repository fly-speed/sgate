#!/usr/bin/env python3
"""Build disposable diagnostic binaries through Go overlays, without source edits."""
import argparse,json,pathlib,subprocess,os,hashlib,datetime
p=argparse.ArgumentParser();p.add_argument('--output',default='artifacts/config-noise-soak');p.add_argument('--race',action='store_true');p.add_argument('--managed',action='store_true',help='also instrument acl_master entry points');a=p.parse_args()
repo=pathlib.Path(__file__).resolve().parent.parent
out=(repo/a.output).resolve();out.mkdir(parents=True,exist_ok=True)
replace={}
def overlay(rel,source):
 target=out/(rel.replace('/','_')+'.overlay');target.write_text(source);replace[str(repo/rel)]=str(target)
def edit_once(source,old,new):
 if source.count(old)!=1:
  raise RuntimeError('diagnostic injection anchor changed: '+old)
 return source.replace(old,new)
# Private census functions exist only in the overlay diagnostic builds.
s=edit_once((repo/'internal/tunnel/client.go').read_text(),'return c.run(ctx, listener, &clientSessions{})','state := &clientSessions{}\n configSoakClients.Store(c, state)\n defer configSoakClients.Delete(c)\n return c.run(ctx, listener, state)')
s+='''
var configSoakClients sync.Map
var configSoakServers sync.Map
func ConfigSoakCensus() map[string]int64 {
 v:=map[string]int64{"sessions":0,"streams":0,"pending_bytes":0,"nonces":0,"attached":0,"noise_sent":0,"noise_received":0}
 add:=func(s *session){s.mu.Lock();defer s.mu.Unlock();v["streams"]+=int64(len(s.streams));v["pending_bytes"]+=int64(s.pendingBytes);v["noise_sent"]+=int64(s.noiseSent);v["noise_received"]+=int64(s.noiseReceived)}
 configSoakClients.Range(func(k,x any)bool{state:=x.(*clientSessions);state.mu.Lock();defer state.mu.Unlock();if state.current!=nil {v["sessions"]++;add(state.current)};return true})
 configSoakServers.Range(func(k,x any)bool{s:=k.(*Server);s.mu.Lock();defer s.mu.Unlock();v["sessions"]+=int64(len(s.sessions));v["nonces"]+=int64(len(s.nonces));for _,e:=range s.sessions {if e.inUse {v["attached"]++};add(e.session)};return true})
 return v
}
'''
overlay('internal/tunnel/client.go',s)
s=edit_once((repo/'internal/tunnel/server.go').read_text(),'go s.reap()','configSoakServers.Store(s,true)\n go s.reap()');overlay('internal/tunnel/server.go',s)
metrics=r'''
func configSoakMetrics() func() {
 config:=flag.Lookup("config").Value.String()
 path:=config+".metrics"
 done,finished:=make(chan struct{}),make(chan struct{})
 go func(){defer close(finished);ticker:=time.NewTicker(2*time.Second);defer ticker.Stop()
 sample:=func(phase string){
  if phase!="load" {debug.FreeOSMemory()}
  var m runtime.MemStats;runtime.ReadMemStats(&m)
  stack:=make([]byte,4<<20);n:=runtime.Stack(stack,true)
  deadline:=time.Now().Add(5*time.Second);for phase=="closed" && (bytes.Count(stack[:n],[]byte(".(*session).writeTransport("))!=0 || runtime.NumGoroutine()>6) && time.Now().Before(deadline){time.Sleep(5*time.Millisecond);n=runtime.Stack(stack,true)}
  if phase=="closed" {if e:=os.WriteFile(path+".closed-"+strconv.Itoa(os.Getpid())+".stack",stack[:n],0600);e!=nil{panic(e)}}
  fd:=-1;if entries,e:=os.ReadDir("/dev/fd");e==nil {fd=len(entries)}
  if fd<0 && phase!="load" {if out,e:=exec.Command("lsof","-a","-p",strconv.Itoa(os.Getpid()),"-Ff").Output();e==nil {fd=0;for _,line:=range strings.Split(string(out),"\n"){if strings.HasPrefix(line,"f"){if _,e:=strconv.Atoi(strings.TrimPrefix(line,"f"));e==nil{fd++}}}}}
  rss:=int64(-1);if b,e:=exec.Command("ps","-o","rss=","-p",strconv.Itoa(os.Getpid())).Output();e==nil {if v,e:=strconv.ParseInt(strings.TrimSpace(string(b)),10,64);e==nil {rss=v*1024}}
  v:=map[string]any{"phase":phase,"unix_nano":time.Now().UnixNano(),"pid":os.Getpid(),"heap_bytes":m.HeapAlloc,"heap_inuse":m.HeapInuse,"rss_bytes":rss,"goroutines":runtime.NumGoroutine(),"fds":fd,"transport_writers":bytes.Count(stack[:n],[]byte(".(*session).writeTransport(")),"stack_truncated":n==len(stack),"census":tunnel.ConfigSoakCensus(),"noise_min":flag.Lookup("noise-interval-min").Value.String(),"noise_max":flag.Lookup("noise-interval-max").Value.String(),"noise_min_bytes":flag.Lookup("noise-min-bytes").Value.String(),"noise_max_bytes":flag.Lookup("noise-max-bytes").Value.String()}
  b,e:=json.Marshal(v);if e!=nil{panic(e)}
  f,e:=os.OpenFile(path,os.O_CREATE|os.O_APPEND|os.O_WRONLY,0600);if e!=nil{panic(e)};_,e=f.Write(append(b,10));f.Close();if e!=nil{panic(e)}
 }
 sample("startup")
 for {select {case <-done:sample("closed");return;case <-ticker.C:
  phase:="load";if b,e:=os.ReadFile(config+".collect");e==nil {phase=string(b);os.Remove(config+".collect")};sample(phase)
 }}
 }()
 return func(){close(done);<-finished}
}
'''
for role in ['client','server']:
 s=(repo/f'cmd/sgate-{role}/main.go').read_text()
 s=edit_once(s,'import (','import (\n "bytes"\n "encoding/json"\n "runtime"\n "runtime/debug"\n "os/exec"\n "strconv"\n "strings"')
 if a.managed:s=edit_once(s,'if aclservice.Enabled() {','if aclservice.Enabled() {\n stopMetrics:=configSoakMetrics()\n defer stopMetrics()')
 if a.managed:s=edit_once(s,'if *adminConfig != "" {','if *adminConfig != "" {\n stopMetrics:=configSoakMetrics()\n defer stopMetrics()')
 if role=='client':s=edit_once(s,'listener, err := net.Listen', 'stopMetrics:=configSoakMetrics()\n defer stopMetrics()\n listener, err := net.Listen')
 else:s=edit_once(s,'defer proxy.Close()','stopMetrics:=configSoakMetrics()\n defer func(){proxy.Close();stopMetrics()}()')
 overlay(f'cmd/sgate-{role}/main.go',s+'\n'+metrics)
mapfile=out/'overlay.json';mapfile.write_text(json.dumps({'Replace':replace},indent=2))
env=os.environ.copy();env.update(GOMAXPROCS='4',GOCACHE='/private/tmp/sgate-go-cache',GOPROXY='off')
for role in ['client','server']:
 cmd=['go','build','-overlay',str(mapfile)]
 if a.race:cmd+=['-race']
 cmd+=['-o',str(out/('sgate-'+role+('-race' if a.race else ''))),'./cmd/sgate-'+role]
 subprocess.run(cmd,cwd=repo,env=env,check=True)
print('built diagnostic binaries:',out,'race=',a.race)

manifest={"created_utc":datetime.datetime.now(datetime.timezone.utc).isoformat(),"race":a.race,"instrumented":True,"overlay":str(mapfile),"sources":{},"binaries":{}}
for original,generated in replace.items():
 manifest["sources"][str(pathlib.Path(original).relative_to(repo))]={"original_sha256":hashlib.sha256(pathlib.Path(original).read_bytes()).hexdigest(),"overlay_sha256":hashlib.sha256(pathlib.Path(generated).read_bytes()).hexdigest()}
for role in ['client','server']:
 f=out/('sgate-'+role+('-race' if a.race else ''));manifest["binaries"][f.name]=hashlib.sha256(f.read_bytes()).hexdigest()
(out/('manifest-race.json' if a.race else 'manifest-plain.json')).write_text(json.dumps(manifest,indent=2)+'\n')
