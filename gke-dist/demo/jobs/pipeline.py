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
from kubeflow.spark import Driver, Executor, SparkClient
from kubeflow.trainer import CustomTrainer, TrainerClient
from kubeflow.trainer.options import kubernetes as k8s_options

NAMESPACE = os.environ.get("DEMO_NAMESPACE", "default")
BUCKET_NAME = os.environ.get("DEMO_BUCKET", "sizhang-gke-dev-ml-demo-data")
DATA_MOUNT = "/data"
DEMO_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# TPU slice selector — matches the multi-host ComputeClass in tpu-compute-class.yaml.
TPU_NODE_SELECTOR = {"cloud.google.com/compute-class": "tpu-v5-8-multi-host"}
TPU_TOLERATIONS = [
    {"key": "google.com/tpu", "operator": "Exists", "effect": "NoSchedule"},
    {"key": "cloud.google.com/compute-class", "operator": "Exists", "effect": "NoSchedule"},
]

# GCS Volume definition for GCSFuse
GCS_VOLUME = {
    "name": "gcs-data",
    "csi": {
        "driver": "gcsfuse.csi.storage.gke.io",
        "volumeAttributes": {"bucketName": BUCKET_NAME},
    },
}
GCS_MOUNT = {"name": "gcs-data", "mountPath": DATA_MOUNT}
GCS_ANNOTATION = {
    "gke-gcsfuse/volumes": "true",
    "gke-gcsfuse/mount-options": "dir-mode=0777,file-mode=0777",
}

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
    """Return a RuntimePatch that mounts GCS via GCSFuse AND pins pods to TPU nodes."""
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
                                        metadata={"annotations": GCS_ANNOTATION},
                                        spec=k8s_options.PodSpecPatch(
                                            node_selector=TPU_NODE_SELECTOR,
                                            tolerations=TPU_TOLERATIONS,
                                            volumes=[GCS_VOLUME],
                                            containers=[{
                                                "name": "node",
                                                "volumeMounts": [GCS_MOUNT],
                                            }],
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


def _gcs_fuse_patch(cr, backend):
    """Custom Spark SDK option to patch GCSFuse volumes into the SparkConnect CR.

    This ensures both the Spark server (driver) and executors can access GCS
    as a local filesystem at /data.
    """
    from kubeflow_spark_api import models

    def patch_template(template):
        if not template:
            template = models.IoK8sApiCoreV1PodTemplateSpec()
        if not template.metadata:
            template.metadata = models.IoK8sApimachineryPkgApisMetaV1ObjectMeta()
        if not template.metadata.annotations:
            template.metadata.annotations = {}
        template.metadata.annotations.update(GCS_ANNOTATION)

        if not template.spec:
            template.spec = models.IoK8sApiCoreV1PodSpec(containers=[])
        if not template.spec.volumes:
            template.spec.volumes = []

        # Add GCS volume
        template.spec.volumes.append(
            models.IoK8sApiCoreV1Volume(
                name="gcs-data",
                csi=models.IoK8sApiCoreV1CSIVolumeSource(
                    driver="gcsfuse.csi.storage.gke.io",
                    volume_attributes={
                        "bucketName": BUCKET_NAME,
                        "mountOptions": "implicit-dirs,dir-mode=0777,file-mode=0777",
                    },
                ),
            )
        )

        # Add volume mount to all containers (usually just one)
        if not template.spec.containers:
            template.spec.containers = [models.IoK8sApiCoreV1Container(name="spark-kubernetes-driver")]
        for c in template.spec.containers:
            if not c.volume_mounts:
                c.volume_mounts = []
            c.volume_mounts.append(
                models.IoK8sApiCoreV1VolumeMount(
                    name="gcs-data", mount_path=DATA_MOUNT
                )
            )
            if not c.env:
                c.env = []
            c.env.append(
                models.IoK8sApiCoreV1EnvVar(name="PYTHONPATH", value=DATA_MOUNT)
            )
        return template

    cr.spec.server.template = patch_template(cr.spec.server.template)
    cr.spec.executor.template = patch_template(cr.spec.executor.template)


def run_data_processing(num_executors=4, num_shards=4, wait=True):
    """Stage 1: run distributed preprocessing using the Kubeflow Spark SDK.

    This replaces the YAML-based SparkApplication submission with a native
    Python call and uses GCS (via GCSFuse) as the data bus.
    """
    from jobs.data_processing import run_etl

    # Sync jobs package to shared volume so executors can import jobs
    import shutil
    target_jobs = os.path.join(DATA_MOUNT, "jobs")
    os.makedirs(target_jobs, exist_ok=True)
    src_jobs = os.path.join(DEMO_DIR, "jobs")
    if os.path.exists(src_jobs):
        for item in os.listdir(src_jobs):
            s = os.path.join(src_jobs, item)
            d = os.path.join(target_jobs, item)
            if os.path.isfile(s):
                shutil.copyfile(s, d)

    print(f"[pipeline] connecting to Spark ({num_executors} executors) via GCS bucket {BUCKET_NAME}...")

    client = SparkClient(backend_config=KubernetesBackendConfig(namespace=NAMESPACE))

    # We use a custom callable option to patch GCSFuse into the SparkConnect pods.
    spark = client.connect(
        num_executors=num_executors,
        driver=Driver(image=SPARK_IMAGE, resources={"cpu": "1", "memory": "2Gi"}),
        executor=Executor(
            num_instances=num_executors,
            resources_per_executor={"cpu": "1", "memory": "2Gi"},
        ),
        options=[_gcs_fuse_patch],
        spark_conf={
            "spark.kubernetes.container.image": SPARK_IMAGE,
            "spark.kubernetes.driver.label.sidecar.istio.io/inject": "false",
            "spark.kubernetes.executor.label.sidecar.istio.io/inject": "false",
            "spark.kubernetes.executor.env.PYTHONPATH": "/data",
            "spark.kubernetes.driver.env.PYTHONPATH": "/data",
        },
    )

    try:
        print(f"[pipeline] running ETL logic ({num_shards} shards)...")
        run_etl(spark, data_root=DATA_MOUNT, num_shards=num_shards)
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

    Reads the preprocessed shards from /data/processed and writes the trained
    model + metrics to /data/model.
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
                "DATA_ROOT": DATA_MOUNT,
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
