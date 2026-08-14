#!/usr/bin/env python3
"""Distributed Data Processing Stage (Stage 1) - Apache Spark on WikiText.

This is the ETL stage, run as a real **Apache Spark** job on Kubernetes.
It processes a WikiText dataset, tokenizes the text using GPT2Tokenizer,
chunks the tokens into blocks of `block_size`, and writes training shards to GCS.
"""

import json
import os
import subprocess
import sys
import time
import urllib.request

os.environ["HF_HOME"] = "/tmp/huggingface"


def _ensure_deps():
    try:
        import numpy  # noqa: F401
        from google.cloud import storage  # noqa: F401
        import pyarrow  # noqa: F401
        import transformers  # noqa: F401
        import tqdm
        return
    except ImportError:
        pass
    target = "/tmp/pydeps"
    os.makedirs(target, exist_ok=True)
    if target not in sys.path:
        sys.path.insert(0, target)
    subprocess.check_call(
        [sys.executable, "-m", "pip", "install", "--quiet",
         "--target", target, "numpy", "google-cloud-storage", "pyarrow", "transformers", "tqdm"]
    )


_ensure_deps()
import numpy as np


def download_file(url, dest):
    if os.path.exists(dest):
        return
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    print(f"Downloading {url} to {dest}...", flush=True)
    req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
    for attempt in range(5):
        try:
            with urllib.request.urlopen(req) as response, open(dest, "wb") as out_file:
                out_file.write(response.read())
            return
        except Exception as e:
            print(f"  download attempt {attempt + 1} failed: {e}", flush=True)
            time.sleep(2 ** attempt)
    raise RuntimeError(f"Failed to download {url}")


def run_etl(spark, bucket_name, raw_dir="/tmp/raw", num_shards=2, num_records=None, block_size=128):
    from pyspark.sql import functions as F
    import pandas as pd

    print(f"[driver] Spark {spark.version}, target shards={num_shards}", flush=True)

    from google.cloud import storage
    client = storage.Client()
    bucket = client.bucket(bucket_name)
    blobs_to_delete = list(bucket.list_blobs(prefix="processed/"))
    if blobs_to_delete:
        print(f"[driver] cleaning up {len(blobs_to_delete)} old shards in GCS...", flush=True)
        bucket.delete_blobs(blobs_to_delete)

    # Create a DataFrame with just the shard indices (0 to num_shards - 1)
    df = spark.range(0, num_shards).repartition(num_shards)

    def process_shard(iterator):
        import os
        os.environ["HF_HOME"] = "/tmp/huggingface"
        _ensure_deps()
        import pandas as pd
        import numpy as np
        import uuid
        from google.cloud import storage
        from transformers import GPT2Tokenizer
        import urllib.request

        tokenizer = GPT2Tokenizer.from_pretrained("gpt2")
        
        base_url = "https://huggingface.co/datasets/JeanKaddour/minipile/resolve/refs%2Fconvert%2Fparquet/default/train"
        MAX_SHARDS = 12

        all_results = []
        
        for pdf in iterator:
            for shard_id in pdf["id"]:
                actual_id = shard_id % MAX_SHARDS
                url = f"{base_url}/{actual_id:04d}.parquet"
                tmp_parquet = f"/tmp/shard-{shard_id}.parquet"
                
                # Download the parquet file directly on the executor
                req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
                with urllib.request.urlopen(req) as response, open(tmp_parquet, "wb") as out_file:
                    out_file.write(response.read())
                
                # Read the parquet file
                shard_df = pd.read_parquet(tmp_parquet)
                if num_records is not None and num_records > 0:
                    per_shard = num_records // num_shards
                    start_idx = (shard_id // MAX_SHARDS) * per_shard
                    shard_df = shard_df.iloc[start_idx : start_idx + per_shard]
                    
                all_tokens = []
                for text in shard_df["text"]:
                    if text and isinstance(text, str) and text.strip():
                        tokens = tokenizer.encode(text)
                        all_tokens.extend(tokens)
                        
                if os.path.exists(tmp_parquet):
                    os.remove(tmp_parquet)
                
                if not all_tokens:
                    continue

                seq_len = block_size + 1
                num_blocks = len(all_tokens) // seq_len
                if num_blocks == 0:
                    continue
                    
                all_tokens = all_tokens[:num_blocks * seq_len]
                data = np.array(all_tokens, dtype=np.uint16).reshape((num_blocks, seq_len))
                
                images = data[:, :-1]
                labels = data[:, 1:]

                uid = str(uuid.uuid4())[:8]
                tmp_path = f"/tmp/shard-{uid}.npz"
                np.savez_compressed(tmp_path, images=images, labels=labels)

                client = storage.Client()
                bucket = client.bucket(bucket_name)
                blob = bucket.blob(f"processed/train/shard-{uid}.npz")
                blob.upload_from_filename(tmp_path)

                if os.path.exists(tmp_path):
                    os.remove(tmp_path)

                res_df = pd.DataFrame({"shard": [uid], "count": [num_blocks]})
                res_df["shard"] = res_df["shard"].astype(str)
                res_df["count"] = res_df["count"].astype("int64")
                all_results.append(res_df)
                
        if not all_results:
            empty_df = pd.DataFrame({
                "shard": pd.Series([], dtype="string"), 
                "count": pd.Series([], dtype="int64")
            })
            return iter([empty_df])
        return iter(all_results)

    print("[driver] launching distributed executors to download and process shards...", flush=True)
    # Use mapInPandas to execute the logic on the cluster
    counts_df = df.mapInPandas(
        process_shard,
        schema="shard string, count long"
    )
    counts = counts_df.collect()

    total = sum(row["count"] for row in counts)
    for row in counts:
        print(f"[executor] shard-{row['shard']}.npz -> {row['count']} blocks generated", flush=True)
    print(f"[driver] wrote {total} train blocks across {len(counts)} shards", flush=True)

    # Generate a small test set on the driver
    print("[driver] generating test set on the driver...", flush=True)
    from transformers import GPT2Tokenizer
    import urllib.request
    import pandas as pd
    import os
    os.environ["HF_HOME"] = "/tmp/huggingface"
    tokenizer = GPT2Tokenizer.from_pretrained("gpt2")
    
    test_url = "https://huggingface.co/datasets/JeanKaddour/minipile/resolve/refs%2Fconvert%2Fparquet/default/test/0000.parquet"
    tmp_test_parquet = "/tmp/test.parquet"
    req = urllib.request.Request(test_url, headers={"User-Agent": "Mozilla/5.0"})
    with urllib.request.urlopen(req) as response, open(tmp_test_parquet, "wb") as out_file:
        out_file.write(response.read())
        
    test_df = pd.read_parquet(tmp_test_parquet)
    test_df = test_df.head(1000)
    
    test_tokens = []
    for text in test_df["text"]:
        if text and isinstance(text, str) and text.strip():
            test_tokens.extend(tokenizer.encode(text))
            
    if os.path.exists(tmp_test_parquet):
        os.remove(tmp_test_parquet)
    
    seq_len = block_size + 1
    num_blocks = len(test_tokens) // seq_len
    from google.cloud import storage
    client = storage.Client()
    bucket = client.bucket(bucket_name)

    if num_blocks > 0:
        test_tokens = test_tokens[:num_blocks * seq_len]
        test_data = np.array(test_tokens, dtype=np.uint16).reshape((num_blocks, seq_len))
        test_x = test_data[:, :-1]
        test_y = test_data[:, 1:]
        
        tmp_test = "/tmp/test.npz"
        np.savez_compressed(tmp_test, images=test_x, labels=test_y)
        blob = bucket.blob("processed/test/test.npz")
        blob.upload_from_filename(tmp_test)
        if os.path.exists(tmp_test):
            os.remove(tmp_test)
        print(f"[driver] wrote {test_x.shape[0]} test blocks", flush=True)
    else:
        print("[driver] Not enough test data to form a block")

    tmp_success = "/tmp/_SUCCESS"
    with open(tmp_success, "w") as f:
        f.write(f"num_shards={len(counts)}\ntrain_examples={total}\n")
    blob = bucket.blob("processed/_SUCCESS")
    blob.upload_from_filename(tmp_success)
    if os.path.exists(tmp_success):
        os.remove(tmp_success)
    print("[driver] Spark ETL complete: wrote _SUCCESS marker", flush=True)


def main():
    from pyspark.sql import SparkSession
    bucket_name = os.environ.get("DEMO_BUCKET")
    if not bucket_name:
        raise ValueError("DEMO_BUCKET environment variable must be set")
    num_shards = int(os.environ.get("NUM_SHARDS", "10"))
    num_records_env = os.environ.get("NUM_RECORDS")
    num_records = int(num_records_env) if num_records_env and num_records_env.isdigit() else None
    block_size = int(os.environ.get("BLOCK_SIZE", "128"))
    
    spark = SparkSession.builder.appName("wikitext-etl").getOrCreate()
    try:
        run_etl(spark, bucket_name=bucket_name, num_shards=num_shards, num_records=num_records, block_size=block_size)
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
