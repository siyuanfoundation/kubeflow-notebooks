#!/usr/bin/env python3
"""Verification Script for Kubeflow Workspace to Kubeflow Trainer JAX TPU Execution.

Usage:
  python3 verify_tpu_trainer.py

This script:
1. Deletes the TPU reservation placeholder job `tpu-job-ccc`.
2. Triggers the JAX TPU TrainJob execution from inside the CPU Jupyter Workspace pod (`tpu-notebook`).
3. Verifies that all 8 TPU cores across 2 hosts are allocated and JAX pmap completes.
4. Automatically re-applies the `tpu-job-ccc` placeholder job to safeguard TPU quota.
"""

import os
import subprocess
import sys
import time

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
PLACEHOLDER_JOB_SPEC = os.path.join(SCRIPT_DIR, "tpu-job-ccc.yaml")
TEST_SCRIPT_PATH = os.path.join(SCRIPT_DIR, "test_jax_tpu.py")
NAMESPACE = "default"


def get_workspace_pod():
    cmd = [
        "kubectl",
        "get",
        "pods",
        "-l",
        "notebooks.kubeflow.org/workspace-name=tpu-notebook",
        "-n",
        NAMESPACE,
        "-o",
        "jsonpath={.items[0].metadata.name}",
    ]
    res = subprocess.run(cmd, capture_output=True, text=True, check=True)
    pod = res.stdout.strip()
    if not pod:
        raise RuntimeError("No running workspace notebook pod found!")
    return pod


def main():
    print("=== Step 1: Locating Workspace Pod ===")
    workspace_pod = get_workspace_pod()
    print(f"Target Workspace Pod: {workspace_pod}")

    print("\n=== Step 2: Deleting TPU Reservation Placeholder Job ===")
    subprocess.run(
        [
            "kubectl",
            "delete",
            "job",
            "tpu-job-ccc",
            "-n",
            NAMESPACE,
            "--ignore-not-found",
        ],
        check=True,
    )
    time.sleep(3)

    try:
        print("\n=== Step 3: Copying Test Script into Workspace Pod ===")
        subprocess.run(
            [
                "kubectl",
                "cp",
                TEST_SCRIPT_PATH,
                f"{NAMESPACE}/{workspace_pod}:/home/jovyan/test_jax_tpu.py",
                "-c",
                "main",
            ],
            check=True,
        )

        print("\n=== Step 4: Executing JAX TPU Training Job from Notebook Pod ===")
        exec_cmd = [
            "kubectl",
            "exec",
            workspace_pod,
            "-n",
            NAMESPACE,
            "-c",
            "main",
            "--",
            "python",
            "/home/jovyan/test_jax_tpu.py",
        ]
        proc = subprocess.run(exec_cmd, capture_output=True, text=True)
        print("--- Execution Output ---")
        print(proc.stdout)
        if proc.stderr:
            print("--- Standard Error ---", file=sys.stderr)
            print(proc.stderr, file=sys.stderr)

        if proc.returncode != 0:
            raise RuntimeError(f"Job execution failed with exit code {proc.returncode}")

        if "Global device count: 8" in proc.stdout and "PMAP result:" in proc.stdout:
            print("\n✅ SUCCESS: Multi-host TPU TrainJob executed successfully from workspace notebook!")
        else:
            raise RuntimeError("Job log output missing expected JAX TPU device count or PMAP result.")

    finally:
        print("\n=== Step 5: Safeguard - Re-applying TPU Reservation Placeholder Job ===")
        subprocess.run(["kubectl", "apply", "-f", PLACEHOLDER_JOB_SPEC], check=True)
        print("Restored tpu-job-ccc placeholder job successfully.")


if __name__ == "__main__":
    main()
