#!/usr/bin/env python3
"""Distributed Model Training Stage (Stage 2) - miniGPT on TPU.

Data-parallel JAX training across a multi-host Cloud TPU slice, launched by
Kubeflow Trainer as a `TrainJob`, or run locally inside the notebook for debugging.

Pipeline contract:
  reads : processed/train/shard-*.npz
          processed/test/test.npz
  writes: model/params.npz
          model/checkpoint-<step>.npz
          model/metrics.json
"""

import os
import sys
import time
import json
import subprocess
import numpy as np

try:
    from google.cloud import storage
except ImportError:
    subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "google-cloud-storage"])
    from google.cloud import storage

try:
    import flax
except ImportError:
    subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "flax", "optax"])

import jax
import jax.numpy as jnp
from jax import grad, pmap, random
import optax
from flax import linen as nn

def train_scaling_model(is_local_debug=False):
    import subprocess
    import sys
    try:
        from google.cloud import storage
    except ImportError:
        print("Installing google-cloud-storage...", flush=True)
        subprocess.check_call([sys.executable, "-m", "pip", "install", "google-cloud-storage"])
        from google.cloud import storage
    
    import os
    import json
    import time
    import numpy as np
    import jax
    import jax.numpy as jnp
    from jax import random, pmap
    from flax import linen as nn
    import flax.serialization
    import optax

    class CausalSelfAttention(nn.Module):
        n_head: int
        n_embd: int
        
        @nn.compact
        def __call__(self, x):
            B, T, C = x.shape
            qkv = nn.Dense(3 * self.n_embd, name="c_attn")(x)
            q, k, v = jnp.split(qkv, 3, axis=-1)
            q = q.reshape(B, T, self.n_head, C // self.n_head).transpose(0, 2, 1, 3)
            k = k.reshape(B, T, self.n_head, C // self.n_head).transpose(0, 2, 1, 3)
            v = v.reshape(B, T, self.n_head, C // self.n_head).transpose(0, 2, 1, 3)
            
            att = (q @ k.transpose(0, 1, 3, 2)) * (1.0 / jnp.sqrt(k.shape[-1]))
            
            mask = jnp.tril(jnp.ones((T, T))).reshape((1, 1, T, T))
            att = jnp.where(mask == 0, -1e9, att)
            att = nn.softmax(att, axis=-1)
            
            y = (att @ v).transpose(0, 2, 1, 3).reshape(B, T, C)
            y = nn.Dense(self.n_embd, name="c_proj")(y)
            return y

    class MLP(nn.Module):
        n_embd: int
        
        @nn.compact
        def __call__(self, x):
            x = nn.Dense(4 * self.n_embd, name="c_fc")(x)
            x = nn.gelu(x, approximate=True)
            x = nn.Dense(self.n_embd, name="c_proj")(x)
            return x

    class Block(nn.Module):
        n_head: int
        n_embd: int
        
        @nn.compact
        def __call__(self, x):
            x = x + CausalSelfAttention(self.n_head, self.n_embd)(nn.LayerNorm(name="ln_1")(x))
            x = x + MLP(self.n_embd)(nn.LayerNorm(name="ln_2")(x))
            return x

    class GPT(nn.Module):
        vocab_size: int
        block_size: int
        n_layer: int
        n_head: int
        n_embd: int
        
        @nn.compact
        def __call__(self, idx):
            B, T = idx.shape
            pos = jnp.arange(0, T, dtype=jnp.int32)
            
            tok_emb = nn.Embed(self.vocab_size, self.n_embd, name="wte")(idx)
            pos_emb = nn.Embed(self.block_size, self.n_embd, name="wpe")(pos)
            x = tok_emb + pos_emb
            
            for i in range(self.n_layer):
                x = Block(self.n_head, self.n_embd, name=f"h_{i}")(x)
                
            x = nn.LayerNorm(name="ln_f")(x)
            logits = nn.Dense(self.vocab_size, name="lm_head")(x)
            return logits


    bucket_name = os.environ.get("BUCKET_NAME")
    if not bucket_name:
        raise ValueError("BUCKET_NAME environment variable must be set")
    epochs = int(os.environ.get("EPOCHS", "2"))
    lr = float(os.environ.get("LEARNING_RATE", "3e-4"))
    global_batch = int(os.environ.get("GLOBAL_BATCH_SIZE", "64"))
    block_size = int(os.environ.get("BLOCK_SIZE", "128"))
    vocab_size = int(os.environ.get("VOCAB_SIZE", "50257"))
    n_layer = int(os.environ.get("N_LAYER", "4"))
    n_head = int(os.environ.get("N_HEAD", "4"))
    n_embd = int(os.environ.get("N_EMBD", "128"))

    process_id = 0
    if not is_local_debug:
        import jax.distributed as dist
        raw_pid = os.environ.get("JAX_PROCESS_ID", "")
        if not raw_pid.isdigit():
            raw_pid = os.environ.get("JOB_COMPLETION_INDEX", "0")
        pid_val = int(raw_pid) if raw_pid.isdigit() else 0

        dist.initialize(
            coordinator_address=os.environ["JAX_COORDINATOR_ADDRESS"],
            num_processes=int(os.environ["JAX_NUM_PROCESSES"]),
            process_id=pid_val,
        )
        process_id = jax.process_index()

    n_local = jax.local_device_count()
    n_global = jax.device_count()

    print(f"[proc {process_id}] JAX Init: local TPU cores={n_local}, global cores={n_global}", flush=True)

    client = storage.Client()
    bucket = client.bucket(bucket_name)

    xs, ys = [], []
    if is_local_debug:
        print(f"[proc {process_id}] LOCAL DEBUG MODE: Loading single data shard for fast debugging", flush=True)
        blobs = list(bucket.list_blobs(prefix="processed/train/shard-"))
        if not blobs:
            raise FileNotFoundError(f"No processed shards under gs://{bucket_name}/processed/train")
        blob = blobs[0]
        tmp_shard = f"/tmp/{os.path.basename(blob.name)}"
        blob.download_to_filename(tmp_shard)
        d = np.load(tmp_shard)
        xs.append(d["images"])
        ys.append(d["labels"])
        os.remove(tmp_shard)
    else:
        print(f"[proc {process_id}] DISTRIBUTED MODE: Loading all shards from GCS", flush=True)
        blobs = list(bucket.list_blobs(prefix="processed/train/shard-"))
        if not blobs:
            raise FileNotFoundError(f"No processed shards under gs://{bucket_name}/processed/train")
        for blob in sorted(blobs, key=lambda b: b.name):
            tmp_shard = f"/tmp/{os.path.basename(blob.name)}"
            blob.download_to_filename(tmp_shard)
            d = np.load(tmp_shard)
            xs.append(d["images"])
            ys.append(d["labels"])
            os.remove(tmp_shard)

    train_x = np.concatenate(xs).astype(np.int32)
    train_y = np.concatenate(ys).astype(np.int32)
    print(f"[proc {process_id}] Loaded {train_x.shape[0]} train examples", flush=True)

    tmp_test = "/tmp/test.npz"
    blob_test = bucket.get_blob("processed/test/test.npz")
    if blob_test:
        blob_test.download_to_filename(tmp_test)
        test_d = np.load(tmp_test)
        test_x = jnp.asarray(test_d["images"], dtype=jnp.int32)
        test_y = jnp.asarray(test_d["labels"], dtype=jnp.int32)
        os.remove(tmp_test)
    else:
        test_x = test_y = None

    model = GPT(vocab_size=vocab_size, block_size=block_size, n_layer=n_layer, n_head=n_head, n_embd=n_embd)
    key = random.PRNGKey(0)
    
    dummy_x = jnp.ones((1, block_size), dtype=jnp.int32)
    initial_params = model.init(key, dummy_x)
    
    optimizer = optax.adamw(learning_rate=lr)
    initial_opt_state = optimizer.init(initial_params)

    def replicate(tree):
        return jax.tree_util.tree_map(lambda x: jnp.broadcast_to(x, (n_local,) + x.shape), tree)

    def unreplicate(tree):
        return jax.tree_util.tree_map(lambda x: np.asarray(x[0]), tree)

    dev_params = replicate(initial_params)
    dev_opt_state = replicate(initial_opt_state)

    def loss_fn(p, x, y):
        logits = model.apply(p, x)
        loss = optax.softmax_cross_entropy_with_integer_labels(logits=logits, labels=y).mean()
        return loss

    def train_step_fn(p, opt_st, x, y):
        loss, g = jax.value_and_grad(loss_fn)(p, x, y)
        g = jax.lax.pmean(g, axis_name="i")
        loss = jax.lax.pmean(loss, axis_name="i")
        updates, opt_st = optimizer.update(g, opt_st, p)
        p = optax.apply_updates(p, updates)
        return p, opt_st, loss

    train_step = pmap(train_step_fn, axis_name="i")

    per_core = max(1, global_batch // n_global)
    step_batch = per_core * n_local
    n = train_x.shape[0]
    rng = np.random.default_rng(process_id)

    ckpt_every = int(os.environ.get("CHECKPOINT_EVERY", "50")) if not is_local_debug else 0
    global_step = 0
    t0 = time.time()

    for epoch in range(epochs):
        perm = rng.permutation(n)
        for i in range(0, n - step_batch + 1, step_batch):
            idx = perm[i:i + step_batch]
            xb = train_x[idx].reshape(n_local, per_core, block_size)
            yb = train_y[idx].reshape(n_local, per_core, block_size)
            dev_params, dev_opt_state, loss = train_step(dev_params, dev_opt_state, jnp.asarray(xb), jnp.asarray(yb))
            global_step += 1
            if global_step % 20 == 0 and process_id == 0:
                print(f"  epoch {epoch} step {global_step} loss={float(loss[0]):.4f}", flush=True)
            if ckpt_every and global_step % ckpt_every == 0 and process_id == 0:
                tmp_ckpt = f"/tmp/checkpoint-{global_step}.npz"
                with open(tmp_ckpt, "wb") as f:
                    f.write(flax.serialization.to_bytes(unreplicate(dev_params)))
                bucket.blob(f"model/checkpoint-{global_step}.npz").upload_from_filename(tmp_ckpt)
                os.remove(tmp_ckpt)

        if process_id == 0 and test_x is not None:
            host_params = unreplicate(dev_params)
            test_loss = loss_fn(host_params, test_x[:64], test_y[:64])
            print(f"[proc 0] epoch {epoch} test_loss={float(test_loss):.4f}", flush=True)

    if process_id == 0:
        final = unreplicate(dev_params)
        tmp_params = "/tmp/params.npz"
        with open(tmp_params, "wb") as f:
            f.write(flax.serialization.to_bytes(final))
        bucket.blob("model/params.npz").upload_from_filename(tmp_params)
        os.remove(tmp_params)
        
        metrics = {
            "epochs": epochs,
            "global_batch_size": global_batch,
            "global_tpu_cores": int(n_global),
            "train_examples": int(train_x.shape[0]),
            "wall_time_seconds": round(time.time() - t0, 1),
        }
        if test_x is not None:
            host_params = final
            test_loss = loss_fn(host_params, test_x[:64], test_y[:64])
            metrics["final_test_loss"] = float(test_loss)
            
        tmp_metrics = "/tmp/metrics.json"
        with open(tmp_metrics, "w") as f:
            json.dump(metrics, f, indent=2)
        bucket.blob("model/metrics.json").upload_from_filename(tmp_metrics)
        os.remove(tmp_metrics)
        print(f"[proc 0] TRAINING COMPLETE: {json.dumps(metrics)}", flush=True)

        if is_local_debug:
            print("\n=== Generating text from locally trained model ===")
            import transformers
            tokenizer = transformers.GPT2Tokenizer.from_pretrained("gpt2")
            prompts = [
                "The history of the world is",
                "Machine learning provides",
                "In the future, artificial intelligence will"
            ]
            
            def generate_step(params, context):
                logits = model.apply(params, context)
                return jnp.argmax(logits[0, -1, :])
                
            jit_generate = jax.jit(generate_step)
            for p in prompts:
                input_ids = tokenizer.encode(p)
                context = jnp.array([input_ids], dtype=jnp.int32)
                for _ in range(30):
                    next_token = jit_generate(final, context)
                    context = jnp.concatenate([context, jnp.array([[next_token]])], axis=1)
                output_text = tokenizer.decode(context[0].tolist())
                print(f"Prompt: {p}")
                print(f"Generated: {output_text}\n")

if __name__ == "__main__":
    is_debug = os.environ.get("LOCAL_DEBUG", "false").lower() == "true"
    train_scaling_model(is_local_debug=is_debug)
