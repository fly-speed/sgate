#!/usr/bin/env python3
"""Isolated acl_master integration; never loads or alters installed service .cf files."""
import argparse, concurrent.futures, contextlib, grp, http.cookiejar, json, threading
import os, pathlib, pwd, signal, socket, ssl, subprocess, tempfile, time, urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1]

def write(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(0o600)

def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]

def eventually(fn, timeout=15):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            value = fn()
            if value:
                return value
        except (OSError, ValueError, urllib.error.URLError, AssertionError) as e:
            last = e
        time.sleep(.05)
    raise AssertionError('condition timed out: ' + str(last))

class Admin:
    def __init__(self, base, context):
        self.base = base
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar), urllib.request.HTTPSHandler(context=context))
        self.csrf = ''
    def call(self, path, data=None, method=None, status=200):
        headers = {'Content-Type': 'application/json', 'X-CSRF-Token': self.csrf}
        req = urllib.request.Request(self.base+'/api/'+path, None if data is None else json.dumps(data).encode(), headers, method=method)
        try:
            response = self.opener.open(req, timeout=10)
        except urllib.error.HTTPError as e:
            response = e
        with response:
            body = json.load(response)
            assert response.code == status, (path, response.code, body)
            return body
    def authenticated(self):
        body = self.call('status')
        assert body['authenticated']
        self.csrf = body['csrf']
        return True
    def runtime(self):
        return self.call('config')['runtime']
    def apply(self, edit=lambda text: text+'\n# apply via Web management\n', status=200):
        cfg = self.call('config')
        return self.call('config', {'content': edit(cfg['content']), 'revision': cfg['revision'], 'apply': True}, 'PUT', status)

class FaultRelay:
    """Raw TCP relay: faults interrupt only the WS/WSS transport, never application sockets."""
    def __init__(self, target):
        self.target = target
        self.listener = socket.socket()
        self.listener.bind(('127.0.0.1', 0)); self.listener.listen(); self.listener.settimeout(.1)
        self.port = self.listener.getsockname()[1]
        self.lock = threading.Lock(); self.peers = set(); self.threads = []
        self.done = threading.Event(); self.acceptor = threading.Thread(target=self.accept)
        self.acceptor.start(); self.drops = 0
    def accept(self):
        while not self.done.is_set():
            try:
                c, _ = self.listener.accept()
                try: remote = socket.create_connection(self.target, 2)
                except OSError: c.close(); continue
                with self.lock: self.peers.update([c, remote])
                for source, dest in [(c, remote), (remote, c)]:
                    t = threading.Thread(target=self.copy, args=(source, dest))
                    self.threads.append(t); t.start()
            except socket.timeout: pass
            except OSError: break
    def copy(self, source, dest):
        try:
            while not self.done.is_set():
                b = source.recv(8192)
                if not b: break
                dest.sendall(b)
                time.sleep(.001)
        except OSError: pass
        finally:
            for c in [source, dest]:
                with contextlib.suppress(OSError): c.shutdown(socket.SHUT_RDWR)
                c.close()
                with self.lock: self.peers.discard(c)
    def drop(self):
        with self.lock: peers = list(self.peers)
        for c in peers:
            with contextlib.suppress(OSError): c.shutdown(socket.SHUT_RDWR)
        if peers: self.drops += 1
    def close(self):
        self.done.set(); self.listener.close(); self.acceptor.join(4); self.drop()
        for t in self.threads: t.join(4)
        assert not self.acceptor.is_alive() and not any(t.is_alive() for t in self.threads)
        with self.lock: assert not self.peers


def stress(args, case, output, admin, addresses, relay, ctl, pid, pid_gone, peers, peers_lock):
    """128 application streams share each tunnel; process restart intentionally closes them."""
    samples = []
    def collect(phase):
        # A restarted client creates a new session. Its old server session must
        # expire under session-ttl; it is deliberately retained until then.
        def drained():
            for role in admin:
                path = case/f'app/{role}.conf.metrics'
                if not path.exists(): return False
                lines = path.read_text().splitlines()
                try: v = json.loads(lines[-1])
                except (ValueError, IndexError): return False
                if v['pid'] != pid(role): return False
                if v['census']['streams'] or v['census']['pending_bytes']: return False
            return no_backend()
        eventually(drained, 35)
        for role in admin: write(case/f'app/{role}.conf.collect', phase)
        found = {}
        def poll():
            for role in admin:
                path = case/f'app/{role}.conf.metrics'
                if not path.exists(): continue
                for line in path.read_text().splitlines():
                    try: v = json.loads(line)
                    except ValueError: continue
                    if v['phase'] == phase: found[role] = v
            return len(found) == 2
        eventually(poll, 15)
        for role, v in found.items():
            assert not v['stack_truncated']
            assert v['census']['streams'] == 0 and v['census']['pending_bytes'] == 0, (phase, role, v)
            assert v['transport_writers'] <= 1, (phase, role, v)
            assert v['census']['noise_sent'] > 0 and v['census']['noise_received'] > 0, (phase, role, v)
            v['role'] = role; samples.append(v)
        eventually(lambda: no_backend())
        return found
    def no_backend():
        with peers_lock: return not peers
    def wave(number):
        ready = threading.Barrier(args.concurrency + 1, timeout=30)
        def stream(index):
            payload = (f'wave={number:04d};stream={index:04d};'.encode() + bytes(range(256))) * 480
            with socket.create_connection(('127.0.0.1', addresses['client'][index % 2]), 10) as c:
                c.settimeout(30)
                ready.wait()
                def send():
                    c.sendall(payload); c.shutdown(socket.SHUT_WR)
                with concurrent.futures.ThreadPoolExecutor(max_workers=1) as sender:
                    future = sender.submit(send)
                    got = bytearray()
                    while True:
                        b = c.recv(65536)
                        if not b: break
                        got.extend(b)
                        assert len(got) <= len(payload), ('duplicate data', number, index)
                    future.result()
                assert got == payload, ('integrity', number, index, len(got), len(payload))
            return len(payload)
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
            futures = [pool.submit(stream, index) for index in range(args.concurrency)]
            ready.wait()
            # Each fault waits for the preceding transport to reconnect, then
            # cuts that transport while application readers/writers remain open.
            for _ in range(3):
                eventually(lambda: admin['client'].call('stats')['connections'] == 1, 15)
                time.sleep(.18); relay.drop()
            return sum(f.result() for f in futures)
    time.sleep(.2)
    baseline = collect('baseline')
    total = 0
    for number in range(args.rounds):
        total += wave(number)
        collect(f'idle-{number}')
        # Keep every application stream alive across a real framework process
        # replacement. Such restarts close streams; only transport faults resume.
        held = []
        try:
            for index in range(args.concurrency):
                c = socket.create_connection(('127.0.0.1', addresses['client'][index % 2]), 10)
                held.append(c)
                c.settimeout(15); c.sendall(b'restart-held')
                got = bytearray()
                while len(got) < 12:
                    chunk = c.recv(12-len(got))
                    assert chunk, 'restart probe closed before its payload arrived'
                    got.extend(chunk)
                assert got == b'restart-held'
            role = 'client' if number % 2 == 0 else 'server'
            old = pid(role); ctl(role, 'restart')
            eventually(lambda: admin[role].authenticated() and pid(role) != old, 25)
            eventually(lambda: pid_gone(old), 20)
            for c in held:
                try: assert c.recv(1) == b'', 'old stream survived process restart'
                except ConnectionResetError: pass
            for a in admin.values(): a.authenticated()
        finally:
            for c in held: c.close()
        # Fresh streams after every replacement have new per-stream markers.
        total += wave(1000+number)
        collect(f'restart-idle-{number}')
        print(f'{output.name}: stress cycle {number+1}/{args.rounds} complete', flush=True)
    final = collect('final-idle')
    for role, v in final.items():
        b = baseline[role]
        assert v['goroutines'] <= b['goroutines'] + 8, (role, 'goroutine growth', b, v)
        if b['fds'] >= 0 and v['fds'] >= 0: assert v['fds'] <= b['fds'] + 4, (role, 'fd growth', b, v)
        assert v['heap_bytes'] <= b['heap_bytes'] + 12*1024*1024, (role, 'heap growth', b, v)
    (output/'stress-samples.json').write_text(json.dumps(samples, indent=2)+'\n')
    return dict(rounds=args.rounds, concurrency=args.concurrency, checked_payload_bytes=total,
                transport_faults=relay.drops, framework_restarts=args.rounds,
                persistent_login=True, old_streams_closed=True, idle_resources_bounded=True)

def run(args, secure):
    case = pathlib.Path(tempfile.mkdtemp(prefix='sgate-acl-', dir='/private/tmp' if pathlib.Path('/private/tmp').is_dir() else None))
    output = pathlib.Path(args.output).resolve() / ('WSS' if secure else 'WS')
    output.mkdir(parents=True, exist_ok=True)
    # Preserve logs under the requested output, without storing generated keys/cookies.
    backend = socket.socket(); backend.bind(('127.0.0.1',0)); backend.listen(); backend.settimeout(.2)
    finished = threading.Event(); peers = set(); lock = threading.Lock(); threads = []
    def echo(c):
        try:
            with c:
                while True:
                    b=c.recv(65536)
                    if not b: break
                    c.sendall(b)
        except OSError: pass
        finally:
            with lock: peers.discard(c)
    def accept():
        while not finished.is_set():
            try:
                c,_=backend.accept()
                with lock: peers.add(c)
                thread=threading.Thread(target=echo,args=(c,));threads.append(thread);thread.start()
            except socket.timeout: pass
            except OSError: break
    accept_thread=threading.Thread(target=accept);accept_thread.start()
    owned_pids = set()
    master = None
    services = {}
    admin = {}
    addresses = {role: [port(), port()] for role in ['client','server']}
    admin_ports = {role:port() for role in ['client','server']}
    management = str(case/'var/public/master.sock')
    relay = FaultRelay(('127.0.0.1', addresses['server'][0])) if args.stress else None
    context = ssl.create_default_context()
    for folder in ['conf/service','var/public','var/private','var/pid','var/log','work']:
        (case/folder).mkdir(parents=True,exist_ok=True)
    key = case/'app/sgate.key'; write(key, os.urandom(32).hex())
    cert, tls_key = case/'app/cert.pem', case/'app/tls.key'
    if secure:
        request = case/'app/openssl.cnf'
        write(request,'[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n[dn]\nCN=localhost\n[ext]\nsubjectAltName=IP:127.0.0.1,DNS:localhost\nbasicConstraints=critical,CA:TRUE\n')
        subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','1','-keyout',str(tls_key),'-out',str(cert),'-config',str(request)],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        context.load_verify_locations(str(cert))
    scheme,wire=('https','wss') if secure else ('http','ws')
    for role in ['server','client']:
        binary = str(pathlib.Path(getattr(args,role)).resolve())
        cfg = case/f'app/{role}.conf'
        settings=f'listen = 127.0.0.1:1\nkey-file = {key}\npassword = isolated-proxy-password\nnoise-interval-min = 10ms\nnoise-interval-max = 30ms\nnoise-min-bytes = 128\nnoise-max-bytes = 512\n'
        if role=='client':
            settings+=f'server = {wire}://127.0.0.1:{relay.port if relay else addresses["server"][0]}/tunnel\n'
            if not args.stress: settings+='reconnect-min = 20ms\nreconnect-max = 100ms\n'
            if secure: settings+=f'tls-ca = {cert}\n'
        else:
            settings+=f'target = 127.0.0.1:{backend.getsockname()[1]}\n'
            if not args.stress: settings+='session-ttl = 500ms\n'
            if secure: settings+=f'tls-cert = {cert}\ntls-key = {tls_key}\n'
        if args.stress: settings += 'max-streams = 256\nheartbeat = 1s\nheartbeat-timeout = 30s\n' + ('offline-timeout = 20s\n' if role == 'client' else 'session-ttl = 20s\n')
        write(cfg,settings)
        acfg=case/f'app/{role}-admin.conf'
        write(acfg,f'listen = 127.0.0.1:{admin_ports[role]}\nweb-dir = {ROOT}/web/admin\nusers-file = {role}-users.json\nsession-dir = {role}-sessions\nsetup-token-file = {role}-setup.token\nsession-timeout = 1h\n'+(f'tls-cert = {cert}\ntls-key = {tls_key}\n' if secure else ''))
        cf=case/f'conf/service/{role}.cf';services[role]=cf
        write(cf,f'''service sgate-test-{role} {{
 master_disable = no
 master_service = 127.0.0.1|{addresses[role][0]}, 127.0.0.1|{addresses[role][1]}
 master_type = sock
 master_reuseport = no
 master_status_notify = 0
 master_maxproc = 1
 master_prefork = 1
 master_unpriv = n
 master_chroot = n
 master_private = n
 master_stop_kill = false
 master_stop_wait = true
 master_command = {binary}
 master_args = -config {cfg} -admin-config {acfg}
 master_home = {case}/work
 master_stdout = {output}/{role}.stdout
 master_stderr = {output}/{role}.stderr
 app_use_limit = 0
 app_idle_limit = 0
 app_threads = 0
}}
''')
        admin[role]=Admin(f'{scheme}://127.0.0.1:{admin_ports[role]}',context)
    write(case/'conf/main.cf',f'''owner_user = {pwd.getpwuid(os.getuid()).pw_name}
owner_group = {grp.getgrgid(os.getgid()).gr_name}
master_owner = {pwd.getpwuid(os.getuid()).pw_name}
default_process_limit = 4
queue_directory = {case}/var
pid_file = {case}/var/pid/master.pid
log_file = {output}/master.log
service_directory = {case}/conf/service
service_file =
daemon_directory = {pathlib.Path(args.master).resolve().parent}
manage_addr = {management}
master_notify_addr = {case}/var/public/monitor.sock
waiting_on_stop = true
stop_kill = true
service_throttle_time = 1
file_exts = .cf
scan_subdir = 0
''')
    def ctl(role, action):
        p=subprocess.run([args.ctl,'-s',management,'-f',str(services[role]),'-a',action],stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,timeout=20)
        with (output/'ctl.log').open('a') as f:f.write(role+' '+action+'\n'+p.stdout)
        assert p.returncode==0,p.stdout
    def exchange(round_no, index):
        payload=(f'round={round_no:03d};stream={index:03d};'.encode()+bytes(range(256)))*256
        with socket.create_connection(('127.0.0.1',addresses['client'][index%2]),2) as c:
            c.settimeout(8);c.sendall(payload);c.shutdown(socket.SHUT_WR)
            got=bytearray()
            while True:
                b=c.recv(65536)
                if not b:break
                got.extend(b)
            assert got==payload,(round_no,index,len(got),len(payload))
        return len(payload)
    def duplex(round_no):
        with concurrent.futures.ThreadPoolExecutor(max_workers=32) as pool:
            return sum(pool.map(lambda index:exchange(round_no,index),range(32)))
    def pid(role):
        rt=admin[role].runtime();assert rt['listener_owner']=='acl_master';assert len(rt['listeners'])==2
        owned_pids.add(int(rt['pid']));return int(rt['pid'])
    def pid_gone(p):
        try:os.kill(p,0);return False
        except ProcessLookupError:return True
    result={'secure':secure,'listeners_per_role':2,'concurrency':32,'payload_bytes':0,'framework_root':str(case)}
    try:
        master=subprocess.Popen([args.master,'-c',str(case/'conf'),'-l',str(output/'master.log')],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        eventually(lambda:all(admin[role].call('status')['setup_required'] for role in admin))
        for role,a in admin.items():
            token=(case/f'app/{role}-setup.token').read_text().strip()
            a.call('setup',{'username':'isolated-admin','password':'isolated-management-password','setup_token':token});a.authenticated();pid(role)
        result['payload_bytes']+=duplex(0)
        # Keep a real application connection active during the first Web apply.
        held=socket.create_connection(('127.0.0.1',addresses['client'][0]),2);held.settimeout(5);held.sendall(b'held');assert held.recv(4)==b'held'
        for round_no in range(1,5):
            for role,a in admin.items():
                old=pid(role);a.apply();a.authenticated();assert pid(role)==old
            result['payload_bytes']+=duplex(round_no)
        assert held.recv(1)==b''
        held.close()
        # Framework-owned listener changes are rejected with disk rollback.
        a=admin['client'];before=(case/'app/client.conf').read_bytes();old=pid('client')
        a.apply(lambda text:text.replace('listen = 127.0.0.1:1','listen = 127.0.0.1:2'),409)
        assert (case/'app/client.conf').read_bytes()==before;assert pid('client')==old
        for round_no,role in enumerate(['client','server'],5):
            old=pid(role);ctl(role,'restart');eventually(lambda:admin[role].authenticated() and pid(role)!=old);eventually(lambda:pid_gone(old))
            eventually(lambda:(exchange(round_no,99) is not None))
            result['payload_bytes']+=duplex(round_no)
        # Stop uses acl_master's state-channel EOF, not a synthetic SIGTERM.
        for role in ['client','server']:
            old=pid(role);ctl(role,'stop');eventually(lambda:pid_gone(old));ctl(role,'start');eventually(lambda:admin[role].authenticated() and pid(role)!=old)
        eventually(lambda:(exchange(7,99) is not None))
        result['payload_bytes']+=duplex(7)
        if args.stress:
            result['stress'] = stress(args, case, output, admin, addresses, relay, ctl, pid, pid_gone, peers, lock)
        result.update(status='passed',web_apply_same_pid=True,framework_restart_new_pid=True,persistent_sessions=True,listener_edit_rollback=True,state_channel_stop=True)
    finally:
        for role in ['client','server']:
            if master is not None and master.poll() is None:
                with contextlib.suppress(Exception):ctl(role,'stop')
        # master_ctl acknowledges the stop request before Go's race runtime
        # finishes its exit delay. Keep the parent alive to reap its children.
        deadline=time.monotonic()+15
        while master is not None and master.poll() is None and time.monotonic()<deadline:
            if all(pid_gone(p) for p in owned_pids):break
            time.sleep(.05)
        if master is not None and master.poll() is None:
            master.send_signal(signal.SIGTERM)
            try:master.wait(12)
            except subprocess.TimeoutExpired:master.kill();master.wait()
        if relay is not None: relay.close()
        finished.set();backend.close();accept_thread.join(3)
        with lock:
            for peer in list(peers):peer.close()
        for thread in threads:thread.join(3)
        remaining=[p for p in owned_pids if not pid_gone(p)]
        for p in remaining:
            with contextlib.suppress(ProcessLookupError):os.kill(p,signal.SIGTERM)
        assert not remaining,('child processes retained',remaining)
        for number in sum(addresses.values(),[])+list(admin_ports.values()):
            with socket.socket() as probe:
                probe.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);probe.bind(('127.0.0.1',number))
        assert not accept_thread.is_alive() and not any(thread.is_alive() for thread in threads),'harness thread retained'
        for path in output.glob('*.stderr'):
            assert 'WARNING: DATA RACE' not in path.read_text(),path
        result['ports_released']=True
        result['harness_threads_released']=True
        for path in (case/'app').glob('*.metrics*'):
            (output/path.name).write_bytes(path.read_bytes())
        if args.stress:
            closed = {}
            for path in output.glob('*.metrics'):
                for line in path.read_text().splitlines():
                    v = json.loads(line)
                    if v['phase'] != 'closed': continue
                    assert v['transport_writers'] == 0, ('transport/noise timer retained', v)
                    assert all(v['census'][k] == 0 for k in ['sessions', 'streams', 'pending_bytes', 'nonces', 'attached']), ('closed census', v)
                    assert v['goroutines'] <= 6, ('closed goroutines', v)
                    closed[v['pid']] = v
            assert owned_pids <= closed.keys(), ('missing shutdown census', owned_pids-closed.keys())
            result['closed_process_censuses'] = len(closed)
        (output/'result.json').write_text(json.dumps(result,indent=2)+'\n')
        import shutil;shutil.rmtree(case)
    return result

def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--client',required=True);parser.add_argument('--server',required=True)
    parser.add_argument('--master',default='/opt/soft/acl-master/libexec/acl_master');parser.add_argument('--ctl',default='/opt/soft/acl-master/bin/master_ctl')
    parser.add_argument('--output',required=True)
    parser.add_argument('--stress',action='store_true');parser.add_argument('--concurrency',type=int,default=128);parser.add_argument('--rounds',type=int,default=6)
    args=parser.parse_args();results=[run(args,secure) for secure in [False,True]]
    print(json.dumps(results))
if __name__=='__main__':main()
