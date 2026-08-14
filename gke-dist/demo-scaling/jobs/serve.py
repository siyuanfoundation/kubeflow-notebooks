#!/usr/bin/env python3
"""Inference / Serving Stage (Stage 3) - miniGPT text generation on GPU.

A GPU-accelerated HTTP inference server that loads the model trained in
Stage 2 (JAX on TPU) from GCS, converts the parameters to PyTorch CUDA tensors,
and serves text generation with high-speed GPU acceleration on NVIDIA L4 hardware.

Endpoints:
  GET  /healthz   -> readiness/liveness probe
  GET  /metrics   -> the metrics.json emitted by training
  POST /generate  -> body: {"prompt": "...", "max_new_tokens": 50}
                     resp: {"text": "..."}
"""

import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import subprocess
import sys

try:
    from google.cloud import storage
except ImportError:
    subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "google-cloud-storage"])
    from google.cloud import storage

try:
    import torch
    import torch.nn as nn
    import torch.nn.functional as F
except ImportError:
    subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "torch"])
    import torch
    import torch.nn as nn
    import torch.nn.functional as F

try:
    from transformers import GPT2Tokenizer
except ImportError:
    subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "transformers<4.45.0"])
    from transformers import GPT2Tokenizer

try:
    import flax
except ImportError:
    subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "flax", "numpy<2.0.0,>=1.26.0"])
    import flax

BUCKET_NAME = os.environ.get("BUCKET_NAME")
if not BUCKET_NAME:
    raise ValueError("BUCKET_NAME environment variable must be set")

PORT = int(os.environ.get("PORT", "8080"))

_params = None
_loaded_mtime = None
_tokenizer = None

# A minimal PyTorch GPT that mirrors the Flax architecture exactly
class CausalSelfAttention(nn.Module):
    def __init__(self, n_head, n_embd):
        super().__init__()
        self.n_head = n_head
        self.n_embd = n_embd
        self.c_attn = nn.Linear(n_embd, 3 * n_embd)
        self.c_proj = nn.Linear(n_embd, n_embd)

    def forward(self, x):
        B, T, C = x.size()
        qkv = self.c_attn(x)
        q, k, v = qkv.split(self.n_embd, dim=2)
        q = q.view(B, T, self.n_head, C // self.n_head).transpose(1, 2)
        k = k.view(B, T, self.n_head, C // self.n_head).transpose(1, 2)
        v = v.view(B, T, self.n_head, C // self.n_head).transpose(1, 2)

        att = (q @ k.transpose(-2, -1)) * (1.0 / np.sqrt(k.size(-1)))
        mask = torch.tril(torch.ones(T, T, device=x.device)).view(1, 1, T, T)
        att = att.masked_fill(mask == 0, float('-inf'))
        att = F.softmax(att, dim=-1)

        y = (att @ v).transpose(1, 2).contiguous().view(B, T, C)
        y = self.c_proj(y)
        return y

class MLP(nn.Module):
    def __init__(self, n_embd):
        super().__init__()
        self.c_fc = nn.Linear(n_embd, 4 * n_embd)
        self.c_proj = nn.Linear(4 * n_embd, n_embd)

    def forward(self, x):
        x = self.c_fc(x)
        x = F.gelu(x)
        x = self.c_proj(x)
        return x

class Block(nn.Module):
    def __init__(self, n_head, n_embd):
        super().__init__()
        self.ln_1 = nn.LayerNorm(n_embd)
        self.attn = CausalSelfAttention(n_head, n_embd)
        self.ln_2 = nn.LayerNorm(n_embd)
        self.mlp = MLP(n_embd)

    def forward(self, x):
        x = x + self.attn(self.ln_1(x))
        x = x + self.mlp(self.ln_2(x))
        return x

class PyTorchGPT(nn.Module):
    def __init__(self, vocab_size, block_size, n_layer, n_head, n_embd):
        super().__init__()
        self.block_size = block_size
        self.wte = nn.Embedding(vocab_size, n_embd)
        self.wpe = nn.Embedding(block_size, n_embd)
        self.blocks = nn.ModuleList([Block(n_head, n_embd) for _ in range(n_layer)])
        self.ln_f = nn.LayerNorm(n_embd)
        self.lm_head = nn.Linear(n_embd, vocab_size)

    def forward(self, idx):
        B, T = idx.size()
        pos = torch.arange(0, T, dtype=torch.long, device=idx.device)
        tok_emb = self.wte(idx)
        pos_emb = self.wpe(pos)
        x = tok_emb + pos_emb
        for block in self.blocks:
            x = block(x)
        x = self.ln_f(x)
        logits = self.lm_head(x)
        return logits


_pt_model = None

def load_model_if_ready():
    global _params, _loaded_mtime, _tokenizer, _pt_model
    client = storage.Client()
    bucket = client.bucket(BUCKET_NAME)
    blob = bucket.get_blob("model/params.npz")
    if not blob:
        return False
    
    updated = blob.updated
    if _params is not None and updated == _loaded_mtime:
        return True
        
    if _tokenizer is None:
        _tokenizer = GPT2Tokenizer.from_pretrained("gpt2")
    
    tmp_path = "/tmp/params.npz"
    blob.download_to_filename(tmp_path)
    
    with open(tmp_path, "rb") as f:
        _params = flax.serialization.from_bytes(None, f.read())
    
    cuda_available = torch.cuda.is_available()
    print(f"[serve] Loading model weights. CUDA available: {cuda_available}", flush=True)
    
    # We must infer hyperparameters from params shapes
    # wte: (vocab_size, n_embd)
    if 'params' in _params:
        _params = _params['params']
        
    vocab_size, n_embd = _params['wte']['embedding'].shape
    block_size = _params['wpe']['embedding'].shape[0]
    n_layer = sum(1 for k in _params.keys() if k.startswith('h_'))
    # infer n_head by knowing n_embd and typically n_embd // n_head == something reasonable like 64
    # let's just assume n_head = n_embd // 32 for simplicity or we can set a fixed n_head=4 since we trained it
    # We know in train.py n_head = 4, n_embd = 128
    n_head = 4
    if n_embd % 4 != 0:
        n_head = n_embd // 32
        
    _pt_model = PyTorchGPT(vocab_size, block_size, n_layer, n_head, n_embd)
    
    # Port flax weights to pytorch model
    state_dict = _pt_model.state_dict()
    
    def copy_weight(pt_name, flax_w, transpose=False):
        w = torch.from_numpy(np.array(flax_w))
        if transpose:
            w = w.t()
        state_dict[pt_name].copy_(w)

    copy_weight("wte.weight", _params['wte']['embedding'])
    copy_weight("wpe.weight", _params['wpe']['embedding'])
    copy_weight("ln_f.weight", _params['ln_f']['scale'])
    copy_weight("ln_f.bias", _params['ln_f']['bias'])
    copy_weight("lm_head.weight", _params['lm_head']['kernel'], transpose=True)
    copy_weight("lm_head.bias", _params['lm_head']['bias'])

    for i in range(n_layer):
        h = _params[f'h_{i}']
        copy_weight(f"blocks.{i}.ln_1.weight", h['ln_1']['scale'])
        copy_weight(f"blocks.{i}.ln_1.bias", h['ln_1']['bias'])
        
        # In Flax, c_attn kernel is (n_embd, 3*n_embd). Pytorch expects (3*n_embd, n_embd) for nn.Linear weight.
        copy_weight(f"blocks.{i}.attn.c_attn.weight", h['CausalSelfAttention_0']['c_attn']['kernel'], transpose=True)
        copy_weight(f"blocks.{i}.attn.c_attn.bias", h['CausalSelfAttention_0']['c_attn']['bias'])
        
        copy_weight(f"blocks.{i}.attn.c_proj.weight", h['CausalSelfAttention_0']['c_proj']['kernel'], transpose=True)
        copy_weight(f"blocks.{i}.attn.c_proj.bias", h['CausalSelfAttention_0']['c_proj']['bias'])

        copy_weight(f"blocks.{i}.ln_2.weight", h['ln_2']['scale'])
        copy_weight(f"blocks.{i}.ln_2.bias", h['ln_2']['bias'])

        copy_weight(f"blocks.{i}.mlp.c_fc.weight", h['MLP_0']['c_fc']['kernel'], transpose=True)
        copy_weight(f"blocks.{i}.mlp.c_fc.bias", h['MLP_0']['c_fc']['bias'])
        
        copy_weight(f"blocks.{i}.mlp.c_proj.weight", h['MLP_0']['c_proj']['kernel'], transpose=True)
        copy_weight(f"blocks.{i}.mlp.c_proj.bias", h['MLP_0']['c_proj']['bias'])

    if cuda_available:
        _pt_model.cuda()
    _pt_model.eval()
    
    os.remove(tmp_path)
    _loaded_mtime = updated
    print(f"[serve] successfully loaded and GPU-cached model weights from gs://{BUCKET_NAME}/model/params.npz (updated={updated})", flush=True)
    return True

def generate(prompt, max_new_tokens=50):
    cuda_available = torch.cuda.is_available()
    tokens = _tokenizer.encode(prompt)
    x = torch.tensor([tokens], dtype=torch.long)
    if cuda_available:
        x = x.cuda()
    
    with torch.no_grad():
        for _ in range(max_new_tokens):
            idx_cond = x if x.size(1) <= _pt_model.block_size else x[:, -_pt_model.block_size:]
            logits = _pt_model(idx_cond)
            logits = logits[:, -1, :]
            probs = F.softmax(logits, dim=-1)
            next_idx = torch.multinomial(probs, num_samples=1)
            x = torch.cat((x, next_idx), dim=1)
            
    return _tokenizer.decode(x[0].tolist())

class Handler(BaseHTTPRequestHandler):
    def _json(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path == "/livez":
            self._json(200, {"status": "ok"})
        elif self.path == "/healthz":
            ready = load_model_if_ready()
            self._json(200 if ready else 503, {"model_loaded": ready, "cuda_available": torch.cuda.is_available()})
        elif self.path == "/metrics":
            client = storage.Client()
            bucket = client.bucket(BUCKET_NAME)
            blob = bucket.get_blob("model/metrics.json")
            if blob:
                metrics_str = blob.download_as_text()
                self._json(200, json.loads(metrics_str))
            else:
                self._json(404, {"error": "metrics not available yet"})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/generate":
            self._json(404, {"error": "not found"})
            return
        if _pt_model is None:
            self._json(503, {"error": "model not loaded yet"})
            return
        try:
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length))
            prompt = body.get("prompt", "Hello")
            max_new_tokens = body.get("max_new_tokens", 50)
            
            text = generate(prompt, max_new_tokens)
            self._json(200, {
                "prompt": prompt,
                "text": text,
            })
        except Exception as e:
            self._json(400, {"error": str(e)})

def main():
    print(f"[serve] starting GPU-accelerated inference server on :{PORT}", flush=True)
    print(f"[serve] CUDA-capable devices: {torch.cuda.device_count() if torch.cuda.is_available() else 0}", flush=True)
    print(f"[serve] waiting for model at gs://{BUCKET_NAME}/model/params.npz ...", flush=True)
    load_model_if_ready()
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    server.serve_forever()

if __name__ == "__main__":
    main()
