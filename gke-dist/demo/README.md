# Demo: Kubeflow Notebooks as the Gateway to Kubernetes

A realistic, end-to-end ML workflow driven entirely from a single Kubeflow
Notebook — demonstrating the vision of the notebook as the **developer's gateway
to Kubernetes**, with all of K8s' power (heterogeneous hardware, distributed
jobs, elastic serving) available behind a familiar JupyterLab UI.

This demo builds on the base `gke-dist` deployment (Kubeflow Workspaces v2 +
Kubeflow Trainer v2 on GKE with Cloud TPU and PodSnapshot). If you haven't
deployed that yet, do so first via [`../build_and_deploy_gke.sh`](../build_and_deploy_gke.sh)
and read the [parent README](../README.md).

---

## The story

An ML engineer opens one small JupyterLab Workspace (0.1 CPU) and, without ever
leaving the notebook, runs a full ML lifecycle across the cluster's mixed
hardware:

| Stage | What it does | Runs on | Kubernetes primitive |
|-------|--------------|---------|----------------------|
| **1. Distributed data processing** | Apache Spark ETL over Fashion-MNIST | 1 Spark driver + 4 executors | Spark Operator `SparkApplication` |
| **2. Distributed model training** | Data-parallel JAX training with cross-host gradient all-reduce | 2-host Cloud TPU slice (8 cores) | Kubeflow Trainer `TrainJob` (JobSet) |
| **3. Inference / serving** | Serves the trained model over HTTP | 2 CPU replicas | `Deployment` + `Service` |

The three stages exchange data through a single **ReadWriteMany** volume, so
there's no object-storage plumbing to set up.

Midway through, we demonstrate **Pause & Resume** (GKE PodSnapshot): snapshot the
notebook pod — kernel memory and all — walk away during the long TPU training
job without paying for idle compute, then restore later with in-memory state
fully intact. This is a genuine pain point for ML engineers, solved.

---

## Architecture

```
                    Kubeflow Notebook (this pod, 0.1 CPU)
                    "the gateway to Kubernetes"
                              │
          ┌───────────────────┼───────────────────────┐
          │ submit            │ submit                 │ deploy
          ▼                   ▼                        ▼
  ┌───────────────┐   ┌────────────────┐      ┌──────────────────┐
  │ Stage 1: ETL  │   │ Stage 2: Train │      │ Stage 3: Serve   │
  │ Spark:        │   │ 2 TPU hosts    │      │ 2 CPU replicas   │
  │ 1 drv + 4 exec│   │ (JobSet)       │      │ (Deployment)     │
  │ (SparkApp)    │   │                │      │                  │
  └──────┬────────┘   └───────┬────────┘      └────────┬─────────┘
         │ write               │ read / write           │ read
         ▼                     ▼                        ▼
   ┌─────────────────────────────────────────────────────────┐
   │        Shared ReadWriteMany volume  (/data)              │
   │  raw/  processed/{train,test}/  model/{params,metrics}   │
   └─────────────────────────────────────────────────────────┘
```

---

## File inventory

```
demo/
├── README.md                     # this file
├── ml_workflow_demo.ipynb        # the notebook you run — the whole story
├── jobs/
│   ├── data_processing.py        # Stage 1: PySpark ETL (runs as Spark driver)
│   ├── train.py                  # Stage 2: multi-host TPU JAX training
│   ├── serve.py                  # Stage 3: HTTP inference server (stdlib only)
│   └── pipeline.py               # helpers the notebook uses to submit jobs
├── manifests/
│   ├── shared-storage.yaml       # shared ReadWriteMany PVC (the data bus)
│   ├── demo-workspace.yaml       # the "gateway" Jupyter Workspace + home PVC
│   ├── spark-data-processing.yaml# Stage 1 SparkApplication + code ConfigMap
│   └── inference-service.yaml    # inference Deployment + Service + ConfigMap
└── scripts/
    ├── run_demo.sh               # set up storage + workspace, copy files in
    ├── apply_data_processing.sh  # run Stage 1 (Spark ETL) from the CLI
    ├── apply_inference.sh        # deploy Stage 3 from the CLI
    └── cleanup_demo.sh           # tear down all demo resources
```

---

## Prerequisites

1. The base `gke-dist` stack deployed and healthy (Workspaces v2, Trainer v2,
   the `jax-distributed` ClusterTrainingRuntime, Istio, PodSnapshot).
2. The **Kubeflow Spark Operator** installed. The base
   [`../build_and_deploy_gke.sh`](../build_and_deploy_gke.sh) installs it for you
   (into the `kubeflow` namespace); it watches all namespaces, so
   `SparkApplication`s in `default` are picked up.
3. A GKE cluster with:
   - a multi-host Cloud TPU ComputeClass (`tpu-v5-8-multi-host`, see
     [`../tpu-compute-class.yaml`](../tpu-compute-class.yaml)),
   - the **GCP Filestore CSI driver** enabled (for the `standard-rwx` storage
     class). Enable it with:
     ```bash
     gcloud container clusters update CLUSTER_NAME --update-addons=GcpFilestoreCsiDriver=ENABLED
     ```
4. `kubectl` pointed at the cluster.

If your cluster's RWX storage class isn't `standard-rwx`, edit
[`manifests/shared-storage.yaml`](manifests/shared-storage.yaml) accordingly.

---

## Quickstart

### 1. Set up the demo (from your workstation)

```bash
cd gke-dist/demo
./scripts/run_demo.sh
```

This creates the shared volume, launches the `ml-demo-notebook` Workspace, copies
the notebook and `jobs/` package into the pod, and prints the Dashboard URL.

### 2. Open the notebook

Open `https://<INGRESS_IP>/workspaces/`, launch the `ml-demo-notebook`
workspace, then open `demo/ml_workflow_demo.ipynb`.

### 3. Run it top to bottom

The notebook walks through all three stages with narration. When you reach the
**Pause & Resume** section, follow the on-screen instructions to snapshot and
restore the workspace, then confirm kernel state survived.

---

## The Pause & Resume demo (the headline feature)

Long TPU training runs are exactly when you want to step away. With Kubeflow
Workspaces Snapshot & Restore (GKE PodSnapshot), you can:

**From the Dashboard:** open the workspace → **Pause** (snapshots the pod to GCS
and deletes it, so you stop paying) → later **Resume** (restores from the
checkpoint, not a cold restart).

**From the CLI:**
```bash
# Pause (snapshot)
kubectl patch workspace ml-demo-notebook -n default --type merge \
  -p '{"spec":{"paused":true}}'

# Resume (restore) — kernel memory, variables, and running state come back
kubectl patch workspace ml-demo-notebook -n default --type merge \
  -p '{"spec":{"paused":false}}'
```

The notebook stores a random `snapshot_secret` before you pause. After resuming,
one cell confirms the value is still present without re-running anything — proof
the entire kernel state was checkpointed and restored.

---

## Running the pipeline headless (CI / no UI)

Everything the notebook does can be driven from a terminal inside the workspace
pod, which is useful for CI:

```bash
# inside the notebook pod (kubectl exec ... -c main -- bash)
cd /home/jovyan/demo
python -m jobs.pipeline          # runs Stage 1 (Spark ETL) then Stage 2 (training)
```

Or run the stages individually from your workstation:

```bash
cd gke-dist/demo
./scripts/apply_data_processing.sh   # Stage 1: Spark ETL
./scripts/apply_inference.sh         # Stage 3: inference service
```

---

## TPU quota safeguard

The base stack uses [`../tpu-job-ccc.yaml`](../tpu-job-ccc.yaml) as a placeholder
Job to hold the TPU slice while idle. Before Stage 2 runs, release it so the
TrainJob can claim the nodes:

```bash
kubectl delete job tpu-job-ccc -n default --ignore-not-found
```

After training, re-apply it to keep the slice reserved:

```bash
kubectl apply -f ../tpu-job-ccc.yaml
```

The notebook's Stage 2 cell already deletes the placeholder for you.

---

## What each stage demonstrates

- **Stage 1 (data processing)** — a real **Apache Spark** ETL job submitted as a
  `SparkApplication` (Kubeflow Spark Operator). Spark launches a driver + 4
  executors, builds a distributed DataFrame, and runs normalize / one-hot /
  augment transforms in parallel via UDFs, writing one `.npz` shard per
  partition to the shared volume. The PySpark script is delivered via a
  ConfigMap and self-installs numpy — no custom image build.
- **Stage 2 (training)** — real multi-host distributed training: JAX brings up
  its distributed runtime across 2 TPU hosts, `pmap` parallelizes across the 8
  cores, and `jax.lax.pmean` averages gradients across *all* cores on *all*
  hosts. Checkpoints and the final model land on the shared volume.
- **Stage 3 (inference)** — the trained model served from ordinary CPU nodes,
  co-located in the same cluster, reading the model straight off shared storage.
  The serving code uses only the Python standard library + numpy and is injected
  via a ConfigMap (no image build).

---

## Cleanup

```bash
cd gke-dist/demo
./scripts/cleanup_demo.sh
```

This removes the demo workspace, inference service, jobs, and (optionally) the
shared data volume. It leaves the base `gke-dist` stack untouched.

---

## Notes & tuning

- **Dataset:** Fashion-MNIST (60k/10k, 28×28 grayscale, 10 classes) — small
  enough to run fast, real enough to be meaningful.
- **Model:** a 2-layer MLP (784 → 256 → 10). Intentionally small so training
  finishes in minutes while still exercising cross-host collectives.
- **Scaling knobs:** `run_data_processing(num_executors=..., num_shards=...)`,
  `run_training(num_hosts=..., epochs=..., global_batch_size=...)` in the
  notebook / [`jobs/pipeline.py`](jobs/pipeline.py).
- **Images:** override `DEMO_TPU_IMAGE` / `DEMO_CPU_IMAGE` env vars to pin
  specific image versions.
