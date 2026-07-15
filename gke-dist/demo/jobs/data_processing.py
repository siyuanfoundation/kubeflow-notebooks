#!/usr/bin/env python3
"""Distributed Data Processing Stage (Stage 1 of the ML Demo) - Apache Spark.

This is the ETL stage, run as a real **Apache Spark** job on Kubernetes via the
Kubeflow Spark Operator. The notebook submits a `SparkApplication` custom
resource; the Spark Operator launches one driver pod and N executor pods, and
Spark distributes the preprocessing across the executors - the canonical
"big data" pattern, now just another workload on the cluster.

What this job does (Fashion-MNIST: 60k train / 10k test, 28x28 grayscale):
  * reads the raw IDX files (downloaded by the driver on first run),
  * builds a distributed DataFrame of (label, pixels[]) rows,
  * normalizes pixels to [0, 1] and one-hot encodes labels *in parallel*
    across Spark executors,
  * applies a light augmentation (random horizontal flip) to the train set,
  * repartitions into `NUM_SHARDS` partitions and writes one compressed .npz
    shard per partition to the shared ReadWriteMany volume.

Output layout on the shared volume (mounted at DATA_ROOT):
  /data/processed/train/shard-000.npz ... shard-NNN.npz
  /data/processed/test/test.npz
  /data/processed/_SUCCESS

This file runs as the Spark driver's `mainApplicationFile`. It requires PySpark,
which is present in the standard `spark` container images.

Environment variables (set on the SparkApplication driver):
  DATA_ROOT    - mount path of the shared volume (default /data)
  NUM_SHARDS   - number of output shards / target partitions (default 4)
"""

import gzip
import os
import shutil
import struct
import subprocess
import sys
import time
import urllib.request


def _ensure_numpy():
    """The stock `spark` image has no numpy. Install it on demand.

    Called on the driver at startup and on each executor inside the partition
    task, so both sides of the Spark job have numpy available without needing a
    custom container image.
    """
    if "/data" not in sys.path:
        sys.path.insert(0, "/data")
    try:
        import numpy  # noqa: F401
        return
    except ImportError:
        pass
    target = "/tmp/pydeps"
    os.makedirs(target, exist_ok=True)
    if target not in sys.path:
        sys.path.insert(0, target)
    subprocess.check_call(
        [sys.executable, "-m", "pip", "install", "--quiet",
         "--target", target, "numpy"]
    )


_ensure_numpy()
import numpy as np

FASHION_MNIST_BASE = "https://storage.googleapis.com/tensorflow/tf-keras-datasets"
FILES = {
    "train_images": "train-images-idx3-ubyte.gz",
    "train_labels": "train-labels-idx1-ubyte.gz",
    "test_images": "t10k-images-idx3-ubyte.gz",
    "test_labels": "t10k-labels-idx1-ubyte.gz",
}


def download(url, dest):
    if os.path.exists(dest):
        return
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    for attempt in range(5):
        try:
            urllib.request.urlretrieve(url, dest)
            return
        except Exception as e:  # noqa: BLE001 - retry any transient network error
            print(f"  download attempt {attempt + 1} failed: {e}", flush=True)
            time.sleep(2 ** attempt)
    raise RuntimeError(f"Failed to download {url}")


def read_idx_images(path):
    with gzip.open(path, "rb") as f:
        magic, num, rows, cols = struct.unpack(">IIII", f.read(16))
        buf = f.read()
    return np.frombuffer(buf, dtype=np.uint8).reshape(num, rows * cols)


def read_idx_labels(path):
    with gzip.open(path, "rb") as f:
        magic, num = struct.unpack(">II", f.read(8))
        buf = f.read()
    return np.frombuffer(buf, dtype=np.uint8)


def run_etl(spark, data_root="/data", num_shards=4):
    from pyspark.sql import functions as F
    from pyspark.sql.types import (
        ArrayType,
        FloatType,
        IntegerType,
        StructField,
        StructType,
    )

    raw_dir = os.path.join(data_root, "raw")
    out_train = os.path.join(data_root, "processed", "train")
    out_test = os.path.join(data_root, "processed", "test")
    
    # Ensure directories exist. If this fails with PermissionError,
    # the shared volume's permissions need to be fixed (e.g. via fsGroup).
    os.makedirs(out_train, exist_ok=True)
    os.makedirs(out_test, exist_ok=True)

    print(f"[driver] Spark {spark.version}, target shards={num_shards}", flush=True)

    # --- Driver downloads the raw dataset (small; done once). ---
    print("[driver] downloading raw Fashion-MNIST...", flush=True)
    for key, fname in FILES.items():
        download(f"{FASHION_MNIST_BASE}/{fname}", os.path.join(raw_dir, fname))

    train_images = read_idx_images(os.path.join(raw_dir, FILES["train_images"]))
    train_labels = read_idx_labels(os.path.join(raw_dir, FILES["train_labels"]))
    print(f"[driver] loaded {train_images.shape[0]} raw train examples", flush=True)

    # --- Build a distributed DataFrame: (label, pixels[784 uint8]). ---
    schema = StructType([
        StructField("label", IntegerType(), False),
        StructField("pixels", ArrayType(IntegerType()), False),
    ])
    rows = [
        (int(lbl), img.tolist())
        for img, lbl in zip(train_images, train_labels)
    ]
    df = spark.createDataFrame(rows, schema=schema).repartition(num_shards)

    # --- Parallel transforms executed across Spark executors via UDFs. ---
    def normalize_and_flip(pixels, do_flip):
        # Executor side needs numpy
        _ensure_numpy()
        import numpy as np
        arr = np.asarray(pixels, dtype=np.float32) / 255.0
        if do_flip:
            arr = arr.reshape(28, 28)[:, ::-1].reshape(-1)
        return arr.tolist()

    def to_one_hot(label):
        oh = [0.0] * 10
        oh[label] = 1.0
        return oh

    normalize_and_flip.__module__ = "__main__"
    to_one_hot.__module__ = "__main__"

    norm_udf = F.udf(normalize_and_flip, ArrayType(FloatType()))
    onehot_udf = F.udf(to_one_hot, ArrayType(FloatType()))

    processed = (
        df.withColumn("do_flip", F.rand(seed=42) < F.lit(0.5))
          .withColumn("features", norm_udf(F.col("pixels"), F.col("do_flip")))
          .withColumn("target", onehot_udf(F.col("label")))
          .select("features", "target")
    )

    # --- Write one .npz shard per Spark partition to the shared volume. ---
    def write_partition(index, part_iter):
        _ensure_numpy()
        import numpy as np
        import shutil
        import time
        feats, targs = [], []
        for r in part_iter:
            feats.append(r["features"])
            targs.append(r["target"])
        if not feats:
            return iter([(index, 0)])
        images = np.asarray(feats, dtype=np.float32)
        labels = np.asarray(targs, dtype=np.float32)
        tmp_path = f"/tmp/shard-{index:03d}-{int(time.time()*1000)}.npz"
        shard_path = os.path.join(out_train, f"shard-{index:03d}.npz")
        np.savez_compressed(tmp_path, images=images, labels=labels)
        shutil.copyfile(tmp_path, shard_path)
        if os.path.exists(tmp_path):
            os.remove(tmp_path)
        return iter([(index, images.shape[0])])

    try:
        counts = (
            processed.rdd
            .mapPartitionsWithIndex(write_partition)
            .filter(lambda x: x[1] > 0)
            .collect()
        )
    except (NotImplementedError, AttributeError, Exception) as e:
        if "rdd is not implemented" in str(e) or "NOT_IMPLEMENTED" in str(e):
            def write_pandas_partition(pdf_iter):
                _ensure_numpy()
                import numpy as np
                import pandas as pd
                import shutil
                import time
                counts_list = []
                for i, pdf in enumerate(pdf_iter):
                    if pdf.empty:
                        continue
                    feats = np.stack(pdf["features"].values).astype(np.float32)
                    targs = np.stack(pdf["target"].values).astype(np.float32)
                    tmp_path = f"/tmp/shard-{i:03d}-{int(time.time()*1000)}.npz"
                    shard_path = os.path.join(out_train, f"shard-{i:03d}.npz")
                    np.savez_compressed(tmp_path, images=feats, labels=targs)
                    shutil.copyfile(tmp_path, shard_path)
                    if os.path.exists(tmp_path):
                        os.remove(tmp_path)
                    counts_list.append(len(pdf))
                yield pd.DataFrame({"count": counts_list})

            write_pandas_partition.__module__ = "__main__"
            res = processed.mapInPandas(write_pandas_partition, "count int").collect()
            counts = [(i, r["count"]) for i, r in enumerate(res) if r["count"] > 0]
        else:
            raise

    total = sum(c for _, c in counts)
    for idx, c in sorted(counts):
        print(f"[executor] shard-{idx:03d}.npz -> {c} examples", flush=True)
    print(f"[driver] wrote {total} train examples across {len(counts)} shards", flush=True)

    # --- Test set: small, processed on the driver. ---
    test_images = read_idx_images(os.path.join(raw_dir, FILES["test_images"]))
    test_labels = read_idx_labels(os.path.join(raw_dir, FILES["test_labels"]))
    test_x = test_images.astype(np.float32) / 255.0
    test_y = np.zeros((test_labels.shape[0], 10), dtype=np.float32)
    test_y[np.arange(test_labels.shape[0]), test_labels] = 1.0
    tmp_test = "/tmp/test.npz"
    np.savez_compressed(tmp_test, images=test_x, labels=test_y)
    shutil.copyfile(tmp_test, os.path.join(out_test, "test.npz"))
    if os.path.exists(tmp_test):
        os.remove(tmp_test)
    print(f"[driver] wrote {test_x.shape[0]} test examples", flush=True)

    with open(os.path.join(data_root, "processed", "_SUCCESS"), "w") as f:
        f.write(f"num_shards={len(counts)}\ntrain_examples={total}\n")
    print("[driver] data processing complete: wrote _SUCCESS marker", flush=True)


def main():
    from pyspark.sql import SparkSession
    data_root = os.environ.get("DATA_ROOT", "/data")
    num_shards = int(os.environ.get("NUM_SHARDS", "4"))
    
    spark = SparkSession.builder.appName("fashion-mnist-etl").getOrCreate()
    try:
        run_etl(spark, data_root=data_root, num_shards=num_shards)
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
