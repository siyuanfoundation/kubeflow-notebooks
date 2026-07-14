#!/usr/bin/env python3
"""JAX TPU Distributed Training Job Submission Script.

This script runs inside a CPU Jupyter Workspace notebook pod.
It uses the Kubeflow Trainer client SDK (from `kubeflow.trainer`) to launch
a multi-host JAX TPU training job on GKE Cloud TPU hardware.
"""

import os
import sys
import time
from kubeflow.common.types import KubernetesBackendConfig
from kubeflow.trainer import CustomTrainer, TrainerClient
from kubeflow.trainer.options import kubernetes as k8s_options


def get_jax_tpu_dist():
    """Distributed training function executed across TPU host nodes."""
    import os
    import jax
    import jax.distributed as dist
    import jax.numpy as jnp

    # Initialize JAX distributed runtime using environment variables injected by Kubeflow Trainer / JobSet
    dist.initialize(
        coordinator_address=os.environ["JAX_COORDINATOR_ADDRESS"],
        num_processes=int(os.environ["JAX_NUM_PROCESSES"]),
        process_id=int(os.environ["JAX_PROCESS_ID"]),
    )

    print("JAX Distributed Environment on TPU")
    print(f"Local devices: {jax.local_devices()}")
    print(f"Global device count: {jax.device_count()}")
    print(f"Process index: {jax.process_index()}")

    # Perform parallel SPMD calculation across TPU cores
    x = jnp.ones((jax.local_device_count(),))
    p_idx = jnp.array([jax.process_index()] * jax.local_device_count())

    y = jax.pmap(lambda v, p: v * p)(x, p_idx)

    print("PMAP result:", y)
    print("TPU JAX Distributed Job Completed Successfully!")


def main():
    client = TrainerClient(
        backend_config=KubernetesBackendConfig(namespace="default")
    )

    # Node selector matching GKE Cloud TPU v5e multi-host slice ComputeClass
    node_selector = {
        "cloud.google.com/compute-class": "tpu-v5-8-multi-host",
    }

    # Patch JobSet PodSpec to route pods onto TPU nodes with required tolerations
    job_patch = k8s_options.RuntimePatch(
        training_runtime_spec=k8s_options.TrainingRuntimeSpecPatch(
            template=k8s_options.JobSetTemplatePatch(
                spec=k8s_options.JobSetSpecPatch(
                    replicated_jobs=[
                        k8s_options.ReplicatedJobPatch(
                            name="node",
                            template=k8s_options.JobTemplatePatch(
                                spec=k8s_options.JobSpecPatch(
                                    template=k8s_options.PodTemplatePatch(
                                        spec=k8s_options.PodSpecPatch(
                                            node_selector=node_selector,
                                            tolerations=[
                                                {
                                                    "key": "google.com/tpu",
                                                    "operator": "Exists",
                                                    "effect": "NoSchedule",
                                                },
                                                {
                                                    "key": "cloud.google.com/compute-class",
                                                    "operator": "Exists",
                                                    "effect": "NoSchedule",
                                                },
                                            ],
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

    print("Submitting TrainJob for JAX TPU from Jupyter Workspace...")
    job_id = client.train(
        runtime="jax-distributed",
        trainer=CustomTrainer(
            func=get_jax_tpu_dist,
            image="us-docker.pkg.dev/cloud-tpu-images/jax-ai-image/tpu:latest",
            num_nodes=2,
            resources_per_node={
                "google.com/tpu": 4,
            },
            env={
                "JAX_PLATFORMS": "tpu,cpu",
                "ENABLE_PJRT_COMPATIBILITY": "true",
            },
        ),
        options=[job_patch],
    )
    print(f"Created TrainJob: {job_id}")

    print("Waiting for TrainJob status...")
    client.wait_for_job_status(job_id, timeout=300, polling_interval=5)
    print("=== Training Job Logs ===")
    try:
        logs = client.get_job_logs(name=job_id)
        if isinstance(logs, dict):
            for pod_name, pod_log in logs.items():
                print(f"--- Log for {pod_name} ---")
                print(pod_log)
        else:
            print("\n".join(logs))
    except Exception as e:
        print(f"Error fetching logs via client: {e}")


if __name__ == "__main__":
    main()
