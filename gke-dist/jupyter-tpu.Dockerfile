FROM ghcr.io/kubeflow/kubeflow/notebook-servers/jupyter-scipy:v1.10.0

# Install jupyter-ai, langchain-google-genai, kubeflow SDK, and kubernetes SDK
RUN pip install --no-cache-dir \
    jupyter-ai \
    langchain-google-genai \
    "kubeflow[spark]" \
    pyspark \
    kubernetes \
    google-cloud-storage \
    pyarrow && \
    pip install --no-cache-dir torch "torch_xla[tpu]" -f https://storage.googleapis.com/libtpu-releases/index.html && \
    pip install --no-cache-dir "jax[tpu]" -f https://storage.googleapis.com/jax-releases/libtpu_releases.html

# Install gemini-cli (requires root)
USER root
RUN npm install -g @google/gemini-cli

# Create dummy config files in /tmp_home/jovyan/.gemini/ to satisfy jupyter-ai-acp-client check.
# s6-overlay will copy them to /home/jovyan/.gemini/ at startup.
RUN mkdir -p /tmp_home/jovyan/.gemini && \
    echo '{}' > /tmp_home/jovyan/.gemini/settings.json && \
    echo '{}' > /tmp_home/jovyan/.gemini/oauth_creds.json && \
    chown -R 1000:100 /tmp_home/jovyan/.gemini

# Switch back to default user
USER 1000
