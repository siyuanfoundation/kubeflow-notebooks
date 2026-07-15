FROM apache/spark:4.0.1
USER root
RUN apt-get update && \
    apt-get install -y software-properties-common curl && \
    add-apt-repository ppa:deadsnakes/ppa -y && \
    apt-get update && \
    apt-get install -y python3.11 python3.11-dev python3.11-distutils python3.11-venv && \
    python3.11 -m ensurepip && \
    python3.11 -m pip install --no-cache-dir numpy pyspark==4.0.1 pyspark-connect==4.0.1 && \
    ln -sf /usr/bin/python3.11 /usr/bin/python3 && \
    rm -rf /var/lib/apt/lists/*
USER 185
