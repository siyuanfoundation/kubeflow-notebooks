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
# TPU_NODE_SELECTOR is now dynamically generated in _tpu_placement_patch
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


def _tpu_placement_patch(compute_class="tpu-v5-8-multi-host", replicated_job_name="node"):
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
                                            node_selector={"cloud.google.com/compute-class": compute_class},
                                            tolerations=TPU_TOLERATIONS,
                                        )
                                    )
                                )
                            )
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
            client.delete_session(SPARK_APP_NAME)

    return SPARK_APP_NAME


def print_spark_logs():
    """Print the Spark driver logs for the ETL job."""
    v1 = _get_k8s_core_api()
    label_selector = f"sparkoperator.k8s.io/connect-name={SPARK_APP_NAME},spark-role=connect-server"
    
    pods = v1.list_namespaced_pod(NAMESPACE, label_selector=label_selector).items
    if not pods:
        # Fallback for SDK v2 naming
        label_selector = f"kubeflow.org/spark-connect-name={SPARK_APP_NAME},spark-role=driver"
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


def run_training(num_hosts=2, tpu_compute_class="tpu-v5-8-multi-host", epochs=5, global_batch_size=1024, block_size=128, vocab_size=50257, n_layer=4, n_head=4, n_embd=256, wait=True, timeout=3600):
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
        options=[_tpu_placement_patch(compute_class=tpu_compute_class)],
    )
    print(f"[pipeline] created training job: {job_id}")
    if wait:
        print_logs(job_id, follow=True)
        client.wait_for_job_status(job_id, timeout=timeout, polling_interval=10)
    return job_id


def print_logs(job_id, follow=False):
    """Print logs for a TPU TrainJob."""
    client = _client()
    if follow:
        print(f"\n--- Streaming logs for {job_id} ---")
        for logline in client.get_job_logs(name=job_id, follow=True):
            print(logline, end="", flush=True)
    else:
        logs = client.get_job_logs(job_id)
        if isinstance(logs, dict):
            for host, log in logs.items():
                print(f"\n--- {host} ---")
                print(log)
        else:
            for item in logs:
                print(item)


def deploy_inference(wait=True, timeout=600):
    """Stage 4: deploy GPU inference service by creating ConfigMap, Deployment, and Service directly using K8s API."""
    print("[pipeline] deploying GPU inference service...")
    
    # Read the real serve.py code
    serve_py_path = os.path.join(DEMO_DIR, "jobs", "serve.py")
    with open(serve_py_path, "r") as f:
        serve_py_content = f.read()

    core_api = _get_k8s_core_api()
    apps_api = client.AppsV1Api(core_api.api_client)
    
    # 1. Create or Update ConfigMap
    cm_name = "ml-scaling-demo-serve-code"
    config_map = client.V1ConfigMap(
        metadata=client.V1ObjectMeta(
            name=cm_name,
            namespace=NAMESPACE,
            labels={"app.kubernetes.io/part-of": "kubeflow-ml-scaling-demo"}
        ),
        data={"serve.py": serve_py_content}
    )
    
    try:
        core_api.read_namespaced_config_map(name=cm_name, namespace=NAMESPACE)
        core_api.replace_namespaced_config_map(name=cm_name, namespace=NAMESPACE, body=config_map)
    except client.ApiException as e:
        if e.status == 404:
            core_api.create_namespaced_config_map(namespace=NAMESPACE, body=config_map)
        else:
            raise

    # 2. Create or Update Deployment
    deployment_name = "scaling-model-inference"
    deployment = client.V1Deployment(
        metadata=client.V1ObjectMeta(
            name=deployment_name,
            namespace=NAMESPACE,
            labels={
                "app": "scaling-model-inference",
                "app.kubernetes.io/part-of": "kubeflow-ml-scaling-demo"
            }
        ),
        spec=client.V1DeploymentSpec(
            replicas=1,
            selector=client.V1LabelSelector(
                match_labels={"app": "scaling-model-inference"}
            ),
            template=client.V1PodTemplateSpec(
                metadata=client.V1ObjectMeta(
                    labels={
                        "app": "scaling-model-inference",
                        "app.kubernetes.io/part-of": "kubeflow-ml-scaling-demo"
                    }
                ),
                spec=client.V1PodSpec(
                    node_selector={"cloud.google.com/compute-class": "gpu-t4-spot"},
                    tolerations=[
                        client.V1Toleration(key="nvidia.com/gpu", operator="Exists", effect="NoSchedule"),
                        client.V1Toleration(key="cloud.google.com/compute-class", operator="Exists", effect="NoSchedule")
                    ],
                    containers=[
                        client.V1Container(
                            name="server",
                            image="pytorch/pytorch:2.2.2-cuda12.1-cudnn8-runtime",
                            command=["bash", "-c"],
                            args=[
                                "pip install --no-cache-dir google-cloud-storage >/dev/null 2>&1\nexec python /app/serve.py\n"
                            ],
                            env=[
                                client.V1EnvVar(name="BUCKET_NAME", value=BUCKET_NAME),
                                client.V1EnvVar(name="PORT", value="8080")
                            ],
                            ports=[client.V1ContainerPort(container_port=8080)],
                            volume_mounts=[
                                client.V1VolumeMount(name="serve-code", mount_path="/app")
                            ],
                            readiness_probe=client.V1Probe(
                                http_get=client.V1HTTPGetAction(path="/healthz", port=8080),
                                initial_delay_seconds=15,
                                period_seconds=5
                            ),
                            liveness_probe=client.V1Probe(
                                http_get=client.V1HTTPGetAction(path="/livez", port=8080),
                                initial_delay_seconds=600,
                                period_seconds=15
                            ),
                            resources=client.V1ResourceRequirements(
                                requests={"cpu": "2", "memory": "8Gi", "nvidia.com/gpu": "1"},
                                limits={"cpu": "2", "memory": "8Gi", "nvidia.com/gpu": "1"}
                            )
                        )
                    ],
                    volumes=[
                        client.V1Volume(
                            name="serve-code",
                            config_map=client.V1ConfigMapVolumeSource(name=cm_name)
                        )
                    ]
                )
            )
        )
    )

    try:
        apps_api.read_namespaced_deployment(name=deployment_name, namespace=NAMESPACE)
        # Force a rollout restart
        import datetime
        deployment.spec.template.metadata.annotations = {
            "kubectl.kubernetes.io/restartedAt": datetime.datetime.now(datetime.timezone.utc).isoformat()
        }
        apps_api.replace_namespaced_deployment(name=deployment_name, namespace=NAMESPACE, body=deployment)
    except client.ApiException as e:
        if e.status == 404:
            apps_api.create_namespaced_deployment(namespace=NAMESPACE, body=deployment)
        else:
            raise

    # 3. Create or Update Service
    service_name = "scaling-model-inference"
    service = client.V1Service(
        metadata=client.V1ObjectMeta(
            name=service_name,
            namespace=NAMESPACE,
            labels={
                "app": "scaling-model-inference",
                "app.kubernetes.io/part-of": "kubeflow-ml-scaling-demo"
            }
        ),
        spec=client.V1ServiceSpec(
            selector={"app": "scaling-model-inference"},
            ports=[
                client.V1ServicePort(name="http", port=80, target_port=8080)
            ],
            type="ClusterIP"
        )
    )
    
    try:
        existing_svc = core_api.read_namespaced_service(name=service_name, namespace=NAMESPACE)
        service.metadata.resource_version = existing_svc.metadata.resource_version
        service.spec.cluster_ip = existing_svc.spec.cluster_ip
        core_api.replace_namespaced_service(name=service_name, namespace=NAMESPACE, body=service)
    except client.ApiException as e:
        if e.status == 404:
            core_api.create_namespaced_service(namespace=NAMESPACE, body=service)
        else:
            raise

    # 4. Wait for rollout
    if wait:
        print("[pipeline] waiting for inference rollout...")
        start_time = time.time()
        while time.time() - start_time < timeout:
            try:
                dep = apps_api.read_namespaced_deployment(name=deployment_name, namespace=NAMESPACE)
                if dep.status and \
                   dep.status.ready_replicas == dep.spec.replicas and \
                   dep.status.updated_replicas == dep.spec.replicas and \
                   dep.status.replicas == dep.spec.replicas:
                    break
            except client.ApiException:
                pass
            time.sleep(5)
        else:
            print("[pipeline] Warning: Timeout waiting for inference rollout.")
            
    print("[pipeline] Inference service is up.")


if __name__ == "__main__":
    # Runs the full headless pipeline sequentially
    import sys
    bucket = os.environ.get("DEMO_BUCKET")
    if not bucket:
        print("ERROR: DEMO_BUCKET env var required.")
        sys.exit(1)
    
    print("=== [Pipeline] STARTING HEADLESS PIPELINE ===")
    run_data_processing(num_executors=4, num_shards=8, num_records=160000)
    
    core_api = _get_k8s_core_api()
    batch_api = client.BatchV1Api(core_api.api_client)
    crd_api = client.CustomObjectsApi(core_api.api_client)

    print("\n=== [Pipeline] WAITING FOR TPU PLACEHOLDER RELEASE ===")
    try:
        batch_api.delete_namespaced_job(
            name="tpu-job-ccc", 
            namespace="default",
            propagation_policy="Background"
        )
    except client.ApiException as e:
        if e.status != 404:
            print(f"Error deleting TPU placeholder job: {e}")
    
    print("\n=== [Pipeline] STARTING TPU TRAINING JOB ===")
    job = run_training(num_hosts=2, epochs=1, global_batch_size=1024, wait=True)
    
    print("\n=== [Pipeline] RE-APPLYING TPU PLACEHOLDER ===")
    # 1. Create Headless Service
    svc_name = "headless-svc-ccc"
    svc = client.V1Service(
        metadata=client.V1ObjectMeta(name=svc_name),
        spec=client.V1ServiceSpec(
            cluster_ip="None",
            selector={"job-name": "tpu-job-ccc"}
        )
    )
    try:
        existing_svc = core_api.read_namespaced_service(name=svc_name, namespace="default")
        svc.metadata.resource_version = existing_svc.metadata.resource_version
        core_api.replace_namespaced_service(name=svc_name, namespace="default", body=svc)
    except client.ApiException as e:
        if e.status == 404:
            core_api.create_namespaced_service(namespace="default", body=svc)
        else:
            print(f"Error creating service: {e}")
            
    # 2. Create Job
    tpu_job = client.V1Job(
        metadata=client.V1ObjectMeta(name="tpu-job-ccc"),
        spec=client.V1JobSpec(
            backoff_limit=0,
            completions=2,
            parallelism=2,
            completion_mode="Indexed",
            template=client.V1PodTemplateSpec(
                spec=client.V1PodSpec(
                    subdomain=svc_name,
                    restart_policy="Never",
                    node_selector={"cloud.google.com/compute-class": "tpu-v5-8-multi-host"},
                    tolerations=[
                        client.V1Toleration(key="google.com/tpu", operator="Exists", effect="NoSchedule")
                    ],
                    containers=[
                        client.V1Container(
                            name="tpu-job",
                            image="us-docker.pkg.dev/cloud-tpu-images/jax-ai-image/tpu:latest",
                            ports=[
                                client.V1ContainerPort(container_port=8471),
                                client.V1ContainerPort(container_port=8431)
                            ],
                            command=["bash", "-c"],
                            args=["python -c 'import jax; print(\"TPU cores:\", jax.device_count())'\nsleep 432000\n"],
                            resources=client.V1ResourceRequirements(
                                requests={"cpu": "10", "memory": "128Gi", "google.com/tpu": "4"},
                                limits={"cpu": "10", "memory": "128Gi", "google.com/tpu": "4"}
                            )
                        )
                    ]
                )
            )
        )
    )
    try:
        batch_api.read_namespaced_job(name="tpu-job-ccc", namespace="default")
        print("TPU placeholder job already exists.")
    except client.ApiException as e:
        if e.status == 404:
            batch_api.create_namespaced_job(namespace="default", body=tpu_job)
        else:
            print(f"Error creating TPU job: {e}")
    
    print("\n=== [Pipeline] CLEANING UP SPARK CONNECT ===")
    try:
        crd_api.delete_namespaced_custom_object(
            group="sparkoperator.k8s.io",
            version="v1alpha1",
            namespace=NAMESPACE,
            plural="sparkconnects",
            name=SPARK_APP_NAME
        )
    except client.ApiException as e:
        if e.status != 404:
            print(f"Error deleting SparkConnect: {e}")
    
    print("\n=== [Pipeline] HEADLESS PIPELINE COMPLETE ===")
