# About the Code-Server Python Image (with Antigravity)

This file contains notes about the custom Kubeflow Notebooks _Code-Server Python_ image built with **Google Antigravity**, **Gemini Code Assist**, and **Google Cloud Code**.

## Pre-installed VS Code Extensions

This image comes with the following extensions pre-installed:
- **Google Antigravity** (`Google.google-antigravity`) — Google's agentic coding platform for VS Code
- **Gemini Code Assist** (`Google.geminicodeassist`) — AI coding assistant powered by Gemini
- **Google Cloud Code** (`googlecloudtools.cloudcode`) — Google Cloud & Kubernetes tools for VS Code
- **Python** (`ms-python.python`) — Python language support and debugging
- **Jupyter** (`ms-toolsai.jupyter`) — Jupyter Notebook support inside VS Code

Additionally, the **Antigravity CLI (`agy`)** is pre-installed in `/usr/local/bin/agy`.

## Getting Started with Antigravity in VS Code

1. Click the **Antigravity** icon in the Activity Bar on the left sidebar to open the extension panel.
2. Click **Continue with Google** on the Welcome screen to authenticate with your Google / corporate account.
3. Follow the sign-in flow and return to code-server.

## Jupyter Extension Requires HTTPS

Because the Jupyter extension uses [Service Workers](https://developer.mozilla.org/en-US/docs/Web/API/Service_Worker_API), it requires HTTPS to work.
If you access this notebook over HTTP, the Jupyter extension will NOT work unless you access via `localhost` port-forwarding (`kubectl port-forward`) or configure valid HTTPS on your Ingress/Gateway.
