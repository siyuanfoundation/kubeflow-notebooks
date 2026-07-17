#!/usr/bin/env python3
"""Pipeline orchestration helpers used by the demo notebook.

These functions submit each stage of the ML workflow to Kubernetes from inside
the Jupyter Workspace pod. Keeping the logic here (rather than inline in the
notebook) makes the notebook narrative clean and lets the same pipeline be
driven from a plain `python -m` invocation for CI.

Stages
------
1. run_data_processing()  -> Apache Spark job (Spark Operator SparkApplication)
2. run_training()         -> multi-host TPU TrainJob (Kubeflow Trainer)
3. (inference is deployed via kubectl / scripts/apply_inference.sh)

All stages share a single GCS bucket (mounted via GCSFuse) at /data inside every worker pod.
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

# Spark node selector — pins Spark pods to N2 nodes with ample RAM to avoid memory eviction
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


SPARK_APP_NAME = "fashion-mnist-etl"


def _get_k8s_core_api():
    """Initialize and return a Kubernetes CoreV1Api client."""
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()
    return client.CoreV1Api()





def run_data_processing(num_executors=4, num_shards=4, wait=True):
    """Stage 1: run distributed preprocessing using the Kubeflow Spark SDK.

    This replaces the YAML-based SparkApplication submission with a native
    Python call and uses GCS (via GCSFuse) as the data bus.
    """
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
        driver=Driver(image=SPARK_IMAGE, resources={"cpu": "1", "memory": "2Gi"}),
        executor=Executor(
            num_instances=num_executors,
            resources_per_executor={"cpu": "1", "memory": "2Gi"},
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
        print(f"[pipeline] running ETL logic ({num_shards} shards)...")
        run_etl(spark, bucket_name=BUCKET_NAME, num_shards=num_shards)
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


def run_training(num_hosts=2, epochs=5, global_batch_size=1024, wait=True, timeout=1800):
    """Stage 2: launch a multi-host TPU TrainJob for data-parallel training.

    Reads the preprocessed shards from GCS and writes the trained
    model + metrics to GCS.
    """
    from jobs.train import train_fashion_mnist  # local module

    client = _client()
    print(f"[pipeline] submitting TPU TrainJob ({num_hosts} hosts, {epochs} epochs)...")
    job_id = client.train(
        runtime="jax-distributed",
        trainer=CustomTrainer(
            func=train_fashion_mnist,
            image=TPU_IMAGE,
            num_nodes=num_hosts,
            resources_per_node={"google.com/tpu": 4},
            env={
                "JAX_PLATFORMS": "tpu,cpu",
                "ENABLE_PJRT_COMPATIBILITY": "true",
                "BUCKET_NAME": BUCKET_NAME,
                "EPOCHS": str(epochs),
                "GLOBAL_BATCH_SIZE": str(global_batch_size),
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
