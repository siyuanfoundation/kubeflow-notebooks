#!/usr/bin/env python3
"""Pipeline orchestration helpers used by the scaling demo notebook.

These functions submit each stage of the ML workflow to Kubernetes from inside
the Jupyter Workspace pod. Keeping the logic here makes the notebook narrative 
clean and lets the same pipeline be driven from a plain `python -m` invocation.

Stages
------
1. run_data_processing()  -> Apache Spark job (Spark Operator SparkApplication)
2. run_training()         -> multi-host TPU TrainJob (Kubeflow Trainer)
"""

import os
import time

from kubernetes import client, config
from kubeflow.common.types import KubernetesBackendConfig
from kubeflow.spark import Driver, Executor, SparkClient, Name, NodeSelector
from kubeflow.trainer import CustomTrainer, TrainerClient
from kubeflow.trainer.options import kubernetes as k8s_options

NAMESPACE = os.environ.get("DEMO_NAMESPACE", "default")
BUCKET_NAME = os.environ.get("DEMO_BUCKET", "sizhang-gke-dev-ml-demo-data")
DEMO_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Spark node selector — pins Spark pods to N2 nodes
SPARK_NODE_SELECTOR = {"cloud.google.com/machine-family": "n2"}

# TPU slice selector — matches the multi-host ComputeClass in tpu-compute-class.yaml.
TPU_NODE_SELECTOR = {"cloud.google.com/compute-class": "tpu-v5-8-multi-host"}
TPU_TOLERATIONS = [
    {"key": "google.com/tpu", "operator": "Exists", "effect": "NoSchedule"},
    {"key": "cloud.google.com/compute-class", "operator": "Exists", "effect": "NoSchedule"},
]

# Images
REGISTRY = os.environ.get("REGISTRY", "us-west1-docker.pkg.dev/sizhang-gke-dev/sizhang-repo")
TAG = os.environ.get("TAG", "local-gke-dev")
SPARK_IMAGE = os.environ.get("DEMO_SPARK_IMAGE", f"{REGISTRY}/spark-py311:{TAG}")
TPU_IMAGE = os.environ.get(
    "DEMO_TPU_IMAGE", "us-docker.pkg.dev/cloud-tpu-images/jax-ai-image/tpu:latest"
)


def _client():
    return TrainerClient(backend_config=KubernetesBackendConfig(namespace=NAMESPACE))


def _tpu_placement_patch(replicated_job_name="node"):
    """Return a RuntimePatch that pins pods to TPU nodes."""
    return k8s_options.RuntimePatch(
        training_runtime_spec=k8s_options.TrainingRuntimeSpecPatch(
            template=k8s_options.JobSetTemplatePatch(
                spec=k8s_options.JobSetSpecPatch(
                    replicated_jobs=[
                        k8s_options.ReplicatedJobPatch(
                            name=replicated_job_name,
                            template=k8s_options.JobTemplatePatch(
                                spec=k8s_options.JobSpecPatch(
                                    template=k8s_options.PodTemplatePatch(
                                        spec=k8s_options.PodSpecPatch(
                                            node_selector=TPU_NODE_SELECTOR,
                                            tolerations=TPU_TOLERATIONS,
                                        )
                                    )
                                )
                            ),
                        )
                    ]
                )
            )
        )
    )


SPARK_APP_NAME = "scaling-data-etl"


def _get_k8s_core_api():
    """Initialize and return a Kubernetes CoreV1Api client."""
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()
    return client.CoreV1Api()


def run_data_processing(num_executors=4, num_shards=10, num_records=1000000, wait=True, block_size=128):
    """Stage 1: run distributed preprocessing using the Kubeflow Spark SDK."""
    from jobs.data_processing import run_etl

    # Package jobs directory as zip and send to Spark executors
    import shutil
    import tempfile
    
    tmp_dir = tempfile.gettempdir()
    zip_path = os.path.join(tmp_dir, "jobs")
    if os.path.exists(zip_path + ".zip"):
        os.remove(zip_path + ".zip")
    shutil.make_archive(zip_path, "zip", root_dir=DEMO_DIR, base_dir="jobs")
    zip_file = zip_path + ".zip"

    print(f"[pipeline] connecting to Spark ({num_executors} executors) via GCS bucket {BUCKET_NAME}...")

    client = SparkClient(backend_config=KubernetesBackendConfig(namespace=NAMESPACE))

    spark = client.connect(
        num_executors=num_executors,
        driver=Driver(image=SPARK_IMAGE, resources={"cpu": "1", "memory": "4Gi"}),
        executor=Executor(
            num_instances=num_executors,
            resources_per_executor={"cpu": "1", "memory": "4Gi"},
        ),
        options=[
            Name(SPARK_APP_NAME),
            NodeSelector(SPARK_NODE_SELECTOR),
        ],
        spark_conf={
            "spark.kubernetes.container.image": SPARK_IMAGE,
            "spark.kubernetes.driver.label.sidecar.istio.io/inject": "false",
            "spark.kubernetes.executor.label.sidecar.istio.io/inject": "false",
        },
    )
    
    spark.addArtifacts(zip_file, pyfile=True)
    if os.path.exists(zip_file):
        os.remove(zip_file)

    try:
        print(f"[pipeline] running ETL logic ({num_records} records, {num_shards} shards)...")
        run_etl(spark, bucket_name=BUCKET_NAME, num_records=num_records, num_shards=num_shards, block_size=block_size)
        print("[pipeline] Stage 1 complete.")
    finally:
        if wait:
            spark.stop()

    return SPARK_APP_NAME


def print_spark_logs():
    """Print the Spark driver logs for the ETL job."""
    v1 = _get_k8s_core_api()
    label_selector = f"kubeflow.org/spark-connect-name={SPARK_APP_NAME},spark-role=driver"
    
    pods = v1.list_namespaced_pod(NAMESPACE, label_selector=label_selector).items
    if not pods:
        # Fallback for SDK v2 naming
        label_selector = "app.kubernetes.io/name=spark-connect"
        pods = v1.list_namespaced_pod(NAMESPACE, label_selector=label_selector).items

    if not pods:
        print("[pipeline] no Spark driver pod found.")
        return

    driver_pod = pods[0].metadata.name
    try:
        logs = v1.read_namespaced_pod_log(driver_pod, NAMESPACE)
        print(logs)
    except client.ApiException as e:
        print(f"[pipeline] error fetching logs for {driver_pod}: {e}")


def run_training(num_hosts=2, epochs=5, global_batch_size=1024, block_size=128, vocab_size=50257, n_layer=4, n_head=4, n_embd=256, wait=True, timeout=1800):
    """Stage 2: launch a multi-host TPU TrainJob for data-parallel training."""
    from jobs.train import train_scaling_model  # local module

    client = _client()
    print(f"[pipeline] submitting TPU TrainJob ({num_hosts} hosts, {epochs} epochs)...")
    job_id = client.train(
        runtime="jax-distributed",
        trainer=CustomTrainer(
            func=train_scaling_model,
            image=TPU_IMAGE,
            num_nodes=num_hosts,
            resources_per_node={"google.com/tpu": 4},
            env={
                "JAX_PLATFORMS": "tpu,cpu",
                "ENABLE_PJRT_COMPATIBILITY": "true",
                "BUCKET_NAME": BUCKET_NAME,
                "EPOCHS": str(epochs),
                "GLOBAL_BATCH_SIZE": str(global_batch_size),
                "BLOCK_SIZE": str(block_size),
                "VOCAB_SIZE": str(vocab_size),
                "N_LAYER": str(n_layer),
                "N_HEAD": str(n_head),
                "N_EMBD": str(n_embd),
                "LOCAL_DEBUG": "false",
            },
        ),
        options=[_tpu_placement_patch()],
    )
    print(f"[pipeline] created training job: {job_id}")
    if wait:
        client.wait_for_job_status(job_id, timeout=timeout, polling_interval=10)
    return job_id


def print_logs(job_id):
    """Print logs for a TPU TrainJob."""
    client = _client()
    logs = client.get_job_logs(job_id)
    if isinstance(logs, dict):
        for host, log in logs.items():
            print(f"\n--- {host} ---")
            print(log)
    else:
        for item in logs:
            print(item)


def deploy_inference(wait=True, timeout=180):
    """Stage 4: deploy GPU inference service by creating ConfigMap and applying manifests."""
    print("[pipeline] deploying GPU inference service...")
    import subprocess
    
    # 1. Create or update the ConfigMap for serve.py
    serve_py_path = os.path.join(DEMO_DIR, "jobs", "serve.py")
    cm_yaml = subprocess.run([
        "kubectl", "create", "configmap", "ml-scaling-demo-serve-code",
        f"--from-file=serve.py={serve_py_path}",
        "-n", NAMESPACE,
        "--dry-run=client", "-o", "yaml"
    ], capture_output=True, check=True, text=True).stdout
    
    subprocess.run(["kubectl", "apply", "-f", "-"], input=cm_yaml, text=True, check=True)
    
    # 2. Apply the deployment manifest, replacing BUCKET_NAME_PLACEHOLDER
    manifest_path = os.path.join(DEMO_DIR, "manifests", "inference-service.yaml")
    with open(manifest_path, "r") as f:
        manifest_content = f.read()
    manifest_content = manifest_content.replace("BUCKET_NAME_PLACEHOLDER", BUCKET_NAME)
    
    subprocess.run(["kubectl", "apply", "-f", "-"], input=manifest_content, text=True, check=True)
    
    # 3. Rollout restart
    subprocess.run(["kubectl", "rollout", "restart", "deployment/scaling-model-inference", "-n", NAMESPACE], check=True)
    
    # 4. Wait for rollout
    if wait:
        print("[pipeline] waiting for inference rollout...")
        subprocess.run(["kubectl", "rollout", "status", "deployment/scaling-model-inference", "-n", NAMESPACE, f"--timeout={timeout}s"], check=True)
    print("[pipeline] Inference service is up.")


if __name__ == "__main__":
    # Runs the full headless pipeline sequentially
    import sys
    bucket = os.environ.get("DEMO_BUCKET")
    if not bucket:
        print("ERROR: DEMO_BUCKET env var required.")
        sys.exit(1)
    
    print("=== [Pipeline] STARTING HEADLESS PIPELINE ===")
    run_data_processing(num_executors=4, num_shards=10, num_records=1000000)
    
    print("\n=== [Pipeline] WAITING FOR TPU PLACEHOLDER RELEASE ===")
    os.system("kubectl delete job tpu-job-ccc -n default --ignore-not-found")
    
    print("\n=== [Pipeline] STARTING TPU TRAINING JOB ===")
    job = run_training(num_hosts=2, epochs=3, global_batch_size=1024, wait=True)
    print_logs(job)
    
    print("\n=== [Pipeline] RE-APPLYING TPU PLACEHOLDER ===")
    os.system("kubectl apply -f ../tpu-job-ccc.yaml")
    
    print("\n=== [Pipeline] HEADLESS PIPELINE COMPLETE ===")
