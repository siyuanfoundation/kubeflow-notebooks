#!/usr/bin/env python3
"""Inference / Serving Stage (Stage 3 of the ML Demo).

A tiny, dependency-light HTTP inference server that loads the model trained in
Stage 2 from the shared ReadWriteMany volume and serves predictions. Running on
regular CPU nodes (no TPU needed for this small MLP), it demonstrates the third
leg of the ML lifecycle — serving — inside the same Kubeflow / Kubernetes world.

Endpoints:
  GET  /healthz   -> readiness/liveness probe
  GET  /metrics   -> the metrics.json emitted by training
  POST /predict   -> body: {"instances": [[784 floats], ...]}
                     resp: {"predictions": [int, ...], "probabilities": [[...], ...]}

The model file is polled at startup so the server can be deployed *before*
training finishes and will begin serving as soon as params.npz appears.
"""

import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np

DATA_ROOT = os.environ.get("DATA_ROOT", "/data")
MODEL_PATH = os.path.join(DATA_ROOT, "model", "params.npz")
METRICS_PATH = os.path.join(DATA_ROOT, "model", "metrics.json")
PORT = int(os.environ.get("PORT", "8080"))

CLASS_NAMES = [
    "T-shirt/top", "Trouser", "Pullover", "Dress", "Coat",
    "Sandal", "Shirt", "Sneaker", "Bag", "Ankle boot",
]

_params = None
_loaded_mtime = None


def load_model_if_ready():
    """Load (or hot-reload) params.npz if it exists and has changed."""
    global _params, _loaded_mtime
    if not os.path.exists(MODEL_PATH):
        return False
    mtime = os.path.getmtime(MODEL_PATH)
    if _params is not None and mtime == _loaded_mtime:
        return True
    d = np.load(MODEL_PATH)
    _params = {k: d[k].astype(np.float32) for k in d.files}
    _loaded_mtime = mtime
    print(f"[serve] loaded model from {MODEL_PATH} (mtime={mtime})", flush=True)
    return True


def forward(x):
    h = np.maximum(x @ _params["w1"] + _params["b1"], 0.0)
    logits = h @ _params["w2"] + _params["b2"]
    logits -= logits.max(axis=-1, keepdims=True)
    exp = np.exp(logits)
    return exp / exp.sum(axis=-1, keepdims=True)


class Handler(BaseHTTPRequestHandler):
    def _json(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):  # silence default noisy logging
        pass

    def do_GET(self):
        if self.path == "/livez":
            self._json(200, {"status": "ok"})
        elif self.path == "/healthz":
            ready = load_model_if_ready()
            self._json(200 if ready else 503, {"model_loaded": ready})
        elif self.path == "/metrics":
            if os.path.exists(METRICS_PATH):
                with open(METRICS_PATH) as f:
                    self._json(200, json.load(f))
            else:
                self._json(404, {"error": "metrics not available yet"})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/predict":
            self._json(404, {"error": "not found"})
            return
        if not load_model_if_ready():
            self._json(503, {"error": "model not loaded yet"})
            return
        try:
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length))
            x = np.asarray(body["instances"], dtype=np.float32)
            if x.ndim == 1:
                x = x[None, :]
            probs = forward(x)
            preds = probs.argmax(axis=-1).tolist()
            self._json(200, {
                "predictions": preds,
                "labels": [CLASS_NAMES[p] for p in preds],
                "probabilities": probs.round(4).tolist(),
            })
        except Exception as e:  # noqa: BLE001 - surface any request error as 400
            self._json(400, {"error": str(e)})


def main():
    print(f"[serve] starting inference server on :{PORT}", flush=True)
    print(f"[serve] waiting for model at {MODEL_PATH} ...", flush=True)
    # Try an initial load, but start serving immediately — the readiness probe
    # (/healthz) gates traffic until params.npz appears on the shared volume.
    load_model_if_ready()
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    server.serve_forever()


if __name__ == "__main__":
    main()
