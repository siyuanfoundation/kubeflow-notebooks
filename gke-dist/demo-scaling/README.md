# Demo: Scaling ML Workflows with PySpark & Cloud TPUs on GKE (Amazon Reviews)

This demo showcases a highly realistic, production-scale ML workflow managed entirely from a single Kubeflow Notebook. It illustrates the role of Kubeflow Notebooks as the **developer's gateway to Kubernetes**, enabling seamless scale-up of data processing and TPU training.

This demo expands on the base `gke-dist/demo` by introducing:
1. **Dataset Scaling (Distributed Spark)**: Processes the industry-standard **Amazon Reviews 2023** (`raw_review_All_Beauty` subset) dataset from Hugging Face. The Spark job dynamically queries the Hugging Face API for Parquet file URLs, downloads raw reviews, constructs a distributed TF-IDF vectorizer (512 dimensions), and binarizes review ratings into sentiment categories (Label: Positive vs Negative Sentiment) in parallel across executor nodes, writing 10 compressed `.npz` shards directly to GCS.
2. **Compute Scaling (TPU Debugging vs. Multi-Host Training)**:
   - **Interactive local TPU debugging**: The notebook Workspace runs on a single-host TPU slice (`tpu-v5-8-single-host` with 8 cores), allowing you to interactively run JAX and debug your network architecture on a single shard of the data.
   - **Distributed multi-host training**: Once the model code is verified, you submit a distributed `TrainJob` to a multi-host TPU slice (`tpu-v5-8-multi-host` spanning 2 nodes × 4 chips = 8 TPU cores) to train the full-scale classifier MLP on the complete dataset.

---

## Workflow Architecture

```
                    Kubeflow Notebook (Single-Host TPU v5e-8)
                     "interactive playground & control plane"
                              │
          ┌───────────────────┼───────────────────────┐
          │ submit            │ submit                 │ deploy
          ▼                   ▼                        ▼
  ┌───────────────┐   ┌────────────────┐      ┌──────────────────┐
  │ Stage 1: ETL  │   │ Stage 2: Train │      │ Stage 3: Serve   │
  │ Spark:        │   │ 2 TPU hosts    │      │ 2 CPU replicas   │
  │ 1 drv + 4 exec│   │ (JobSet)       │      │ (Deployment)     │
  │ (Spark SDK)   │   │                │      │                  │
  └──────┬────────┘   └───────┬────────┘      └────────┬─────────┘
         │ write               │ read / write           │ read
         ▼                     ▼                        ▼
   ┌─────────────────────────────────────────────────────────┐
   │                  Shared GCS Bucket                      │
   │  processed/train/shard-{0..9}.npz  │  model/params.npz  │
   └─────────────────────────────────────────────────────────┘
```

---

## File Inventory

```
demo-scaling/
├── README.md                      # this file
├── ml_workflow_scaling_demo.ipynb # the Jupyter notebook running the scaling story
├── jobs/
│   ├── __init__.py
│   ├── data_processing.py         # Stage 1: Distributed PySpark TF-IDF vectorizer for Amazon Reviews
│   ├── train.py                   # Stage 2: JAX sentiment classifier (local debug & multi-host)
│   ├── serve.py                   # Stage 3: HTTP serving of predictions
│   └── pipeline.py                # Pipeline submission helpers
├── manifests/
│   ├── demo-workspace-scaling.yaml # gateway Workspace with single-host TPU requested
│   ├── jupyterlab_workspacekind_scaling.yaml # updated WorkspaceKind with tpu_single_host
│   └── inference-service.yaml     # Stage 3 Deployment and Service
└── scripts/
    ├── run_demo.sh                # sets up bucket, patches WorkspaceKind, launches notebook
    ├── apply_data_processing.sh   # triggers Stage 1 Spark ETL from the CLI
    ├── apply_inference.sh         # deploys Stage 3 serving from the CLI
    └── cleanup_demo.sh            # tears down all demo resources
```

---

## Prerequisites

1. The base `gke-dist` stack deployed and healthy.
2. The Kubeflow Spark Operator installed.
3. A GKE cluster with single-host TPU (`tpu-v5-8-single-host`) and multi-host TPU (`tpu-v5-8-multi-host`) ComputeClasses applied.

---

## Quickstart

### 1. Set up the demo
From your local workstation:
```bash
cd gke-dist/demo-scaling
./scripts/run_demo.sh
```
This script ensures the GCS bucket is configured, patches the `jupyterlab` WorkspaceKind on the cluster to include the `tpu_single_host` option, launches the TPU-attached workspace, and copies all demo files into it.

### 2. Open the notebook
Open `https://<INGRESS_IP>/workspaces/`, launch the `ml-scaling-demo-notebook` workspace, and open `demo/ml_workflow_scaling_demo.ipynb`.

### 3. Run the notebook
The notebook is self-documenting and guides you through the stages:
1. **Stage 1 (PySpark)**: Spark connects and downloads raw Amazon review parquet files, processes them into 512-dim TF-IDF features and sentiment labels, and saves them to GCS in parallel.
2. **Local TPU Debugging**: JAX detects the local TPU inside the notebook. You load a single shard (100,000 rows) and train for 1 epoch locally to interactively verify the model and backprop.
3. **Stage 2 (Distributed TPU)**: Release the TPU reservation placeholder and submit the distributed `TrainJob` to train the full-scale MLP classifier on the entire dataset across the multi-host TPU slice.
4. **Stage 3 (Inference)**: Deploy the 2-replica CPU inference server that dynamically loads the trained model from GCS and responds to prediction requests.

### 4. Cleanup
When finished, tear down the demo resources:
```bash
./scripts/cleanup_demo.sh
```
