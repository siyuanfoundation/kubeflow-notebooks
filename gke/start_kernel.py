import urllib.request
import urllib.parse
import json
import http.cookiejar

sj = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(sj))

base_url = 'http://localhost:8888/workspace/connect/default/ws-jupyter-small/jupyterlab/'

# GET to get cookies
req = urllib.request.Request(base_url)
opener.open(req)

# Find _xsrf cookie
xsrf_token = None
for cookie in sj:
    if cookie.name == '_xsrf':
        xsrf_token = cookie.value
        break

if not xsrf_token:
    print("Failed to get _xsrf token")
    exit(1)

# POST to start kernel
data = json.dumps({'name': 'python3'}).encode('utf-8')
headers = {
    'Content-Type': 'application/json',
    'X-XSRFToken': xsrf_token
}
req = urllib.request.Request(base_url + 'api/kernels', data=data, headers=headers)
try:
    res = opener.open(req)
    print(res.read().decode('utf-8'))
except Exception as e:
    print("Failed to start kernel:", e)
