import ssl
import sys
import time
import json
import urllib.request
import http.cookiejar
import subprocess
from websocket import create_connection

gateway_ip = "8.229.100.100"
workspace_name = "ws-jupyter-small"
namespace = "default"
base_url = f"https://{gateway_ip}/workspace/connect/{namespace}/{workspace_name}/jupyterlab/"
ws_url = f"wss://{gateway_ip}/workspace/connect/{namespace}/{workspace_name}/jupyterlab/api/kernels/{{kernel_id}}/channels"

# Disable SSL verification for urllib
ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
urllib.request.install_opener(urllib.request.build_opener(urllib.request.HTTPSHandler(context=ctx)))

# Setup cookie jar
sj = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(sj), urllib.request.HTTPSHandler(context=ctx))

def get_cookies_and_xsrf():
    req = urllib.request.Request(base_url)
    opener.open(req)
    xsrf_token = None
    cookies_str = ""
    for cookie in sj:
        cookies_str += f"{cookie.name}={cookie.value}; "
        if cookie.name == '_xsrf':
            xsrf_token = cookie.value
    return cookies_str, xsrf_token

def start_kernel(cookies_str, xsrf_token):
    data = json.dumps({'name': 'python3'}).encode('utf-8')
    headers = {
        'Content-Type': 'application/json',
        'X-XSRFToken': xsrf_token,
        'Cookie': cookies_str
    }
    req = urllib.request.Request(base_url + 'api/kernels', data=data, headers=headers)
    res = opener.open(req)
    kernel_info = json.loads(res.read().decode('utf-8'))
    return kernel_info['id']

def run_code(kernel_id, cookies_str, code):
    url = ws_url.format(kernel_id=kernel_id)
    # We need to pass cookies in header
    headers = [f"Cookie: {cookies_str}"]
    ws = create_connection(url, sslopt={"cert_reqs": ssl.CERT_NONE}, header=headers)
    
    # Send execute request
    msg_id = "test_msg_1"
    msg = {
        "header": {
            "msg_id": msg_id,
            "username": "username",
            "session": "session",
            "msg_type": "execute_request",
            "version": "5.2"
        },
        "metadata": {},
        "content": {
            "code": code,
            "silent": False,
            "store_history": True,
            "user_expressions": {},
            "allow_stdin": False
        },
        "buffers": [],
        "parent_header": {}
    }
    ws.send(json.dumps(msg))
    
    # Wait for response
    result = None
    while True:
        res = json.loads(ws.recv())
        if res.get('parent_header', {}).get('msg_id') == msg_id:
            msg_type = res.get('msg_type')
            if msg_type == 'execute_result':
                result = res['content']['data']['text/plain']
            elif msg_type == 'stream':
                result = res['content']['text']
            elif msg_type == 'status' and res['content']['execution_state'] == 'idle':
                break
            elif msg_type == 'error':
                result = "ERROR: " + "".join(res['content']['traceback'])
                break
    ws.close()
    return result

def run_cmd(cmd):
    print(f"Running: {cmd}")
    subprocess.run(cmd, shell=True, check=True)

def wait_for_pod_status(status, timeout=120):
    start = time.time()
    while time.time() - start < timeout:
        out = subprocess.check_output(f"kubectl get pods -l notebooks.kubeflow.org/workspace-name={workspace_name} -o json", shell=True)
        pods = json.loads(out.decode('utf-8'))['items']
        if status == "Terminated":
            if len(pods) == 0:
                return True
        elif status == "Running":
            if len(pods) > 0 and all(c.get('ready', False) for c in pods[0].get('status', {}).get('containerStatuses', [])):
                return True
        time.sleep(2)
    raise TimeoutError(f"Timed out waiting for pod status {status}")

def main():
    print("Getting cookies...")
    cookies_str, xsrf_token = get_cookies_and_xsrf()
    print(f"Cookies: {cookies_str}")
    
    print("Starting kernel...")
    kernel_id = start_kernel(cookies_str, xsrf_token)
    print(f"Kernel ID: {kernel_id}")
    
    print("Setting variables (a=100, b=200, c=a*b)...")
    res = run_code(kernel_id, cookies_str, "a = 100\nb = 200\nc = a*b\nprint(c)")
    print(f"Result: {res}")
    if res.strip() != "20000":
        print("Failed to set variables correctly")
        sys.exit(1)
        
    print("Pausing workspace...")
    run_cmd(f"kubectl patch workspace {workspace_name} --type merge -p '{{\"spec\":{{\"paused\":true}}}}'")
    
    print("Waiting for pod to terminate...")
    wait_for_pod_status("Terminated")
    print("Workspace paused.")
    
    # Wait a bit in paused state
    time.sleep(5)
    
    print("Resuming workspace...")
    run_cmd(f"kubectl patch workspace {workspace_name} --type merge -p '{{\"spec\":{{\"paused\":false}}}}'")
    
    print("Waiting for pod to be running and ready...")
    wait_for_pod_status("Running")
    print("Workspace resumed.")
    
    # Wait for Jupyter to stabilize
    time.sleep(5)
    
    print("Getting new cookies (session might have changed)...")
    cookies_str, xsrf_token = get_cookies_and_xsrf()
    
    print("Checking variable c...")
    res = run_code(kernel_id, cookies_str, "print(c)")
    print(f"Result: {res}")
    if res.strip() == "20000":
        print("SUCCESS: State preserved!")
    else:
        print(f"FAILURE: State not preserved. Result: {res}")
        sys.exit(1)

if __name__ == "__main__":
    main()
