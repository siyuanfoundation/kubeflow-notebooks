#!/usr/bin/env python3
"""Distributed Model Training Stage (Stage 2 of the ML Demo).

Data-parallel JAX training across a multi-host Cloud TPU slice, launched by
Kubeflow Trainer as a `TrainJob`. Each TPU host runs this function; JAX's
`pmap` fans the computation out across the local TPU cores, and gradients are
averaged across *all* cores on *all* hosts with `jax.lax.pmean`.

Pipeline contract (shared ReadWriteMany volume mounted at DATA_ROOT):
  reads : /data/processed/train/shard-*.npz   (from Stage 1)
          /data/processed/test/test.npz
  writes: /data/model/params.npz              (final trained weights)
          /data/model/checkpoint-<step>.npz   (periodic checkpoints)
          /data/model/metrics.json            (final accuracy / loss)

The model is a small 2-layer MLP (784 -> 256 -> 10). It is intentionally small
so the demo trains in a couple of minutes while still exercising real
multi-host collective communication.
"""

import glob
import json
import os
import time


def train_fashion_mnist():
    import glob
    import json
    import os
    import time
    import jax
    import jax.distributed as dist
    import jax.numpy as jnp
    import numpy as np
    from jax import grad, pmap
    from jax import random

    raw_pid = os.environ.get("JAX_PROCESS_ID", "")
    if not raw_pid.isdigit():
        raw_pid = os.environ.get("JOB_COMPLETION_INDEX", "0")
    pid_val = int(raw_pid) if raw_pid.isdigit() else 0

    # --- 1. Bring up the JAX distributed runtime across TPU hosts. ---
    dist.initialize(
        coordinator_address=os.environ["JAX_COORDINATOR_ADDRESS"],
        num_processes=int(os.environ["JAX_NUM_PROCESSES"]),
        process_id=pid_val,
    )

    process_id = jax.process_index()
    n_local = jax.local_device_count()
    n_global = jax.device_count()
    data_root = os.environ.get("DATA_ROOT", "/data")
    epochs = int(os.environ.get("EPOCHS", "5"))
    lr = float(os.environ.get("LEARNING_RATE", "0.1"))
    global_batch = int(os.environ.get("GLOBAL_BATCH_SIZE", "1024"))

    print(f"[proc {process_id}] local TPU cores={n_local}, global cores={n_global}", flush=True)

    # --- 2. Load the preprocessed shards produced by Stage 1. ---
    shard_files = sorted(glob.glob(os.path.join(data_root, "processed", "train", "shard-*.npz")))
    if not shard_files:
        raise FileNotFoundError(
            f"No processed shards under {data_root}/processed/train — run Stage 1 first."
        )
    xs, ys = [], []
    for sf in shard_files:
        d = np.load(sf)
        xs.append(d["images"])
        ys.append(d["labels"])
    train_x = np.concatenate(xs).astype(np.float32)
    train_y = np.concatenate(ys).astype(np.float32)
    print(f"[proc {process_id}] loaded {train_x.shape[0]} train examples", flush=True)

    test_d = np.load(os.path.join(data_root, "processed", "test", "test.npz"))
    test_x = jnp.asarray(test_d["images"], dtype=jnp.float32)
    test_y = jnp.asarray(test_d["labels"], dtype=jnp.float32)

    # --- 3. Initialize model parameters and replicate across local cores. ---
    key = random.PRNGKey(0)
    k1, k2 = random.split(key)
    hidden = 256
    params = {
        "w1": random.normal(k1, (784, hidden)) * jnp.sqrt(2.0 / 784),
        "b1": jnp.zeros((hidden,)),
        "w2": random.normal(k2, (hidden, 10)) * jnp.sqrt(2.0 / hidden),
        "b2": jnp.zeros((10,)),
    }

    def replicate(tree):
        return jax.tree_util.tree_map(lambda x: jnp.broadcast_to(x, (n_local,) + x.shape), tree)

    def unreplicate(tree):
        return jax.tree_util.tree_map(lambda x: np.asarray(x[0]), tree)

    dev_params = replicate(params)

    def forward(p, x):
        h = jnp.maximum(x @ p["w1"] + p["b1"], 0.0)
        return h @ p["w2"] + p["b2"]

    def loss_fn(p, x, y):
        logits = forward(p, x)
        logp = logits - jax.scipy.special.logsumexp(logits, axis=-1, keepdims=True)
        return -jnp.mean(jnp.sum(y * logp, axis=-1))

    def train_step_fn(p, x, y):
        # Average gradients across ALL cores on ALL hosts, then apply SGD.
        g = grad(loss_fn)(p, x, y)
        g = jax.lax.pmean(g, axis_name="i")
        loss = jax.lax.pmean(loss_fn(p, x, y), axis_name="i")
        p = jax.tree_util.tree_map(lambda w, gw: w - lr * gw, p, g)
        return p, loss

    train_step = pmap(train_step_fn, axis_name="i")

    # --- 4. Training loop with data-parallel batches. ---
    # Each core gets global_batch / n_global examples per step.
    per_core = max(1, global_batch // n_global)
    step_batch = per_core * n_local  # examples consumed per host per step
    n = train_x.shape[0]
    rng = np.random.default_rng(process_id)

    os.makedirs(os.path.join(data_root, "model"), exist_ok=True)
    ckpt_every = int(os.environ.get("CHECKPOINT_EVERY", "50"))
    global_step = 0
    t0 = time.time()

    for epoch in range(epochs):
        perm = rng.permutation(n)
        for i in range(0, n - step_batch + 1, step_batch):
            idx = perm[i:i + step_batch]
            xb = train_x[idx].reshape(n_local, per_core, 784)
            yb = train_y[idx].reshape(n_local, per_core, 10)
            dev_params, loss = train_step(dev_params, jnp.asarray(xb), jnp.asarray(yb))
            global_step += 1
            if global_step % 20 == 0 and process_id == 0:
                print(f"  epoch {epoch} step {global_step} loss={float(loss[0]):.4f}", flush=True)
            # Rank 0 writes periodic checkpoints to the shared volume.
            if ckpt_every and global_step % ckpt_every == 0 and process_id == 0:
                cpath = os.path.join(data_root, "model", f"checkpoint-{global_step}.npz")
                np.savez(cpath, **unreplicate(dev_params))

        # --- Evaluate on the held-out test set once per epoch (rank 0). ---
        if process_id == 0:
            host_params = unreplicate(dev_params)
            logits = forward({k: jnp.asarray(v) for k, v in host_params.items()}, test_x)
            preds = jnp.argmax(logits, axis=-1)
            labels = jnp.argmax(test_y, axis=-1)
            acc = float(jnp.mean(preds == labels))
            print(f"[proc 0] epoch {epoch} test_accuracy={acc:.4f}", flush=True)

    # --- 5. Persist the final model + metrics to the shared volume (rank 0). ---
    if process_id == 0:
        final = unreplicate(dev_params)
        np.savez(os.path.join(data_root, "model", "params.npz"), **final)
        host_params = {k: jnp.asarray(v) for k, v in final.items()}
        logits = forward(host_params, test_x)
        preds = jnp.argmax(logits, axis=-1)
        labels = jnp.argmax(test_y, axis=-1)
        final_acc = float(jnp.mean(preds == labels))
        metrics = {
            "final_test_accuracy": final_acc,
            "epochs": epochs,
            "global_batch_size": global_batch,
            "global_tpu_cores": int(n_global),
            "train_examples": int(train_x.shape[0]),
            "wall_time_seconds": round(time.time() - t0, 1),
        }
        with open(os.path.join(data_root, "model", "metrics.json"), "w") as f:
            json.dump(metrics, f, indent=2)
        print(f"[proc 0] TRAINING COMPLETE: {json.dumps(metrics)}", flush=True)


if __name__ == "__main__":
    # Allows running the training function directly (e.g. single-host smoke test).
    train_fashion_mnist()
