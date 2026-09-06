#!/usr/bin/env python3
"""Real Docker/PID integration using simulated devices; requires local image,
Docker access and passwordless sudo for the temporary host guard. No real
accelerator is touched. Every Redis key and container belongs to this test.
"""
import argparse
import json
import os
from pathlib import Path
import pwd
import socket
import subprocess as sp
import tempfile
import time


def run(*args, check=True, **kwargs):
    return sp.run([str(x) for x in args], text=True, stdout=sp.PIPE,
                  stderr=sp.PIPE, check=check, **kwargs)


def eventually(fn, timeout=25):
    end = time.monotonic() + timeout
    last = None
    while time.monotonic() < end:
        try:
            result = fn()
            if result:
                return result
        except (AssertionError, ValueError, KeyError, sp.CalledProcessError) as exc:
            last = exc
        time.sleep(.2)
    raise AssertionError(f"condition timed out: {last}")


def stop_guard(guard):
    children = run('pgrep', '-P', guard.pid, check=False).stdout.split()
    for child in children:
        run('sudo', '-n', 'kill', '-TERM', child, check=False)
    try:
        guard.wait(timeout=20)
    except sp.TimeoutExpired:
        for child in children:
            run('sudo', '-n', 'kill', '-KILL', child, check=False)
        guard.wait(timeout=10)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image', required=True, help='Existing Linux image with sh and sleep')
    parser.add_argument('--binary', default='build/canhazgpu-docker')
    args = parser.parse_args()
    binary = Path(args.binary).resolve()
    owner = pwd.getpwuid(os.getuid()).pw_name
    assert os.getuid() != 0, 'Run as your host account; sudo is used only for the test guard'
    other = 'nobody'
    assert owner != other
    run('sudo', '-n', 'true')
    run('docker', 'image', 'inspect', args.image)
    with tempfile.TemporaryDirectory(prefix='chg-docker-') as temp:
        base = Path(temp)
        base.chmod(0o755)
        usage_file = base / 'usage.json'
        usage_file.write_text('[]')
        mock = base / 'nvidia-smi'
        mock.write_text('''#!/usr/bin/python3
import json,sys,os
from pathlib import Path
rows=json.loads(Path(__file__).with_name('usage.json').read_text())
alive=[]
for gpu,pid in rows:
 try:
  stat=Path('/proc/%d/stat'%pid).read_text().rsplit(')',1)[1].split()
  if stat[0]!='Z': alive.append((gpu,pid))
 except (FileNotFoundError,ProcessLookupError): pass
arg=' '.join(sys.argv[1:])
if '--query-gpu=' in arg:
 for gpu in range(2):
  mem=512*sum(g==gpu for g,p in alive)
  print('%d, GPU-test%d, Test GPU, %d, 0'%(gpu,gpu,mem))
elif '--query-compute-apps=' in arg:
 for gpu,pid in alive: print('%d, sleep, GPU-test%d, 512 MiB'%(pid,gpu))
elif '-L' in arg:
 print('GPU 0: Test GPU\\nGPU 1: Test GPU')
''')
        mock.chmod(0o755)
        with socket.socket() as port_socket:
            port_socket.bind(('127.0.0.1', 0))
            port = port_socket.getsockname()[1]
        redis_log = open(base / 'redis.log', 'w')
        guard_log = open(base / 'guard.log', 'w')
        redis = sp.Popen(['redis-server', '--bind', '127.0.0.1', '--port', str(port),
                          '--save', '', '--appendonly', 'no'], stdout=redis_log, stderr=sp.STDOUT)
        owners_file = base / 'owners.json'
        owners_file.write_text('{}')
        containers = []
        guard = None
        try:
            eventually(lambda: run('redis-cli', '-p', port, 'ping', check=False).stdout.strip() == 'PONG')
            run(binary, '--host-socket', 'off', '--redis-port', port, 'admin', '--gpus', '2', '--provider', 'fake')
            run('redis-cli', '-p', port, 'set', 'canhazgpu:provider', 'nvidia')
            guard_argv = ['sudo', '-n', 'env', 'PATH='+temp+':'+os.environ['PATH'], str(binary),
                              'guard', '--redis-port', str(port), '--listen', str(base / 'host.sock'),
                              '--interval', '1s', '--grace', '2s', '--confirmations', '1', '--enforce',
                              '--max-warnings', '1', '--warn-interval', '1s', '--kill-grace', '1s',
                              '--channels', 'log', '--docker-owners', str(owners_file)]
            guard = sp.Popen(guard_argv, stdout=guard_log, stderr=sp.STDOUT)
            eventually(lambda: (base / 'host.sock').exists())
            for account in (owner, other, ''):
                cid = run('docker', 'run', '--rm', '-d', '--network', 'none',
                          '--label', 'canhazgpu.owner='+account,
                          '-v', temp+':/run/canhazgpu:ro',
                          '-v', str(binary)+':/usr/local/bin/canhazgpu:ro',
                          '--entrypoint', '/bin/sh', args.image, '-c', 'exec sleep 600').stdout.strip()
                containers.append(cid)
            a, b, unmapped = containers

            def cli(cid, *argv, **kwargs):
                return run('docker', 'exec', cid, 'canhazgpu', *argv, **kwargs)

            def state(gpu):
                return json.loads(run('redis-cli', '-p', port, 'get', f'canhazgpu:gpu:{gpu}').stdout.strip() or '{}') or {}

            def host(*argv, **kwargs):
                return run(binary, '--host-socket', base / 'host.sock', *argv, **kwargs)

            def start(cid, gpu):
                run('docker', 'exec', '-d', cid, 'canhazgpu', 'run', '--gpu-ids', gpu,
                    '--idle-timeout', '0', '--', 'sleep', '600')
                return eventually(lambda: state(gpu).get('pid') and state(gpu))

            def usage(rows):
                tmp = base / 'usage.next'
                tmp.write_text(json.dumps(rows))
                tmp.replace(usage_file)

            sa = start(a, 0)
            sb = start(b, 1)
            assert sa['actual_user'] == owner and sb['actual_user'] == other
            assert sa['pid'] != sb['pid']
            assert a in Path('/proc/%d/cgroup' % sa['pid']).read_text()
            assert b in Path('/proc/%d/cgroup' % sb['pid']).read_text()
            usage([[0, sa['pid']], [1, sb['pid']]])
            def attributed():
                states = json.loads(cli(a, 'status', '--json', '--no-schedule').stdout)
                return all(s.get('processes') and s['processes'][0]['user'] == u
                           for s, u in zip(states, (owner, other)))
            eventually(attributed)
            for output in (host('status', '--json', '--no-schedule'), cli(b, 'status', '--json', '--no-schedule')):
                states = json.loads(output.stdout)
                assert [s['user'] for s in states] == [owner, other]
                assert not any(s.get('foreign_users') for s in states)
            print('PASS: two root containers retain distinct host owners and host PIDs; status agrees', flush=True)
            missing_owner = cli(unmapped, 'status', check=False)
            assert missing_owner.returncode != 0 and 'needs canhazgpu.owner' in missing_owner.stderr
            owners_file.write_text(json.dumps({unmapped: owner}))
            stop_guard(guard)
            stopped = cli(a, 'status', check=False)
            assert stopped.returncode != 0 and 'connect to host guard' in stopped.stderr
            guard = sp.Popen(guard_argv, stdout=guard_log, stderr=sp.STDOUT)
            eventually(lambda: (base / 'host.sock').exists())
            states = json.loads(cli(unmapped, 'status', '--json', '--no-schedule').stdout)
            assert [s['user'] for s in states] == [owner, other]
            assert Path('/proc/%d' % sa['pid']).exists()
            print('PASS: missing owners rejected; existing container mapping and guard restart work', flush=True)
            assert cli(b, 'cancel', sa['task_id'], check=False).returncode != 0
            assert cli(b, 'cancel', sa['task_id'], '--force', check=False).returncode != 0
            assert Path('/proc/%d' % sa['pid']).exists()
            host('cancel', sa['task_id'])
            cli(b, 'cancel', sb['task_id'])
            eventually(lambda: not state(0).get('user') and not state(1).get('user'))
            print('PASS: host and container cancellation kill container processes; foreign cancellation rejected', flush=True)
            usage([])
            eventually(lambda: all(s['status'] == 'AVAILABLE' for s in json.loads(host('status', '--json', '--no-schedule').stdout)))
            # Finishing a command preserves its exit status, env and supervisor cleanup.
            done = cli(a, 'run', '--gpu-ids', '0', '--idle-timeout', '0', '--',
                       'sh', '-c', 'test "$CUDA_VISIBLE_DEVICES" = 0; exit 7', check=False)
            assert done.returncode == 7, done
            eventually(lambda: not state(0).get('user'))
            # Queue PID is also in host namespace and a different owner can cancel its own queue entry.
            sa = start(a, 0)
            run('docker', 'exec', '-d', b, 'canhazgpu', 'run', '--gpu-ids', '0', '--', 'sleep', '600')
            def queued():
                keys = run('redis-cli', '-p', port, '--scan', '--pattern', 'canhazgpu:queue:entry:*').stdout.splitlines()
                if not keys: return None
                return json.loads(run('redis-cli', '-p', port, 'get', keys[0]).stdout)
            entry = eventually(queued)
            assert entry['actual_user'] == other
            assert b in Path('/proc/%d/cgroup' % entry['pid']).read_text()
            cli(b, 'cancel', entry['id'][:8])
            eventually(lambda: not queued())
            host('cancel', sa['task_id'])
            print('PASS: exit code, visible devices, automatic release, and queued cancellation', flush=True)
            # A bypassing process is actually killed by the host guard.
            run('docker', 'exec', '-d', a, 'sleep', '601')
            def rogue_pid():
                for line in run('docker', 'top', a, '-eo', 'pid,args').stdout.splitlines():
                    fields = line.split(None, 1)
                    if len(fields) == 2 and fields[1] == 'sleep 601': return int(fields[0])
            rogue = eventually(rogue_pid)
            usage([[0, rogue]])
            eventually(lambda: not Path('/proc/%d' % rogue).exists())
            assert owner in (base / 'guard.log').read_text()
            assert 'SIGINT' in (base / 'guard.log').read_text()
            print('PASS: host guard attributes and terminates an unreserved process inside Docker', flush=True)
            # A missing bridge is an error, never a fallback to container-local Redis or PIDs.
            missing = run('docker', 'exec', '-e', 'CANHAZGPU_HOST_SOCKET=/missing.sock', a,
                          'canhazgpu', 'status', check=False)
            assert missing.returncode != 0 and 'connect to host guard' in missing.stderr
            print('PASS: disconnected clients fail explicitly', flush=True)
        except Exception as exc:
            if isinstance(exc, sp.CalledProcessError):
                print(exc.stderr, flush=True)
            guard_log.flush()
            print((base / 'guard.log').read_text(), flush=True)
            raise
        finally:
            for cid in containers:
                run('docker', 'rm', '-f', cid, check=False)
            if guard:
                stop_guard(guard)
            redis.terminate()
            redis.wait(timeout=10)
            guard_log.close()
            redis_log.close()

if __name__ == '__main__':
    main()
