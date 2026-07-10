# Stateful Pause/Resume Development Log (GKE with gVisor)

This log documents the attempts, findings, and final solution for implementing stateful pause/resume for Jupyter notebooks on GKE using PodSnapshot and Restore (gVisor runtime).

## Goal
Preserve the in-memory state (variables, running kernels) of a Jupyter notebook across a workspace pause (scale to 0) and resume (scale to 1) cycle, without needing to re-run previous cells.

---

## Attempts & Results

### Attempt 1: Default TCP Configuration (Baseline Failure)
*   **Approach**: Use standard GKE PodSnapshot on a default Jupyter deployment (which uses TCP loopback for kernel-server communication).
*   **Result**: Pod restore succeeded, but the notebook session lost connection to the kernel, resulting in `NameError: name 'c' is not defined` when trying to access variables.
*   **Reason**: TCP connections (even loopback) are not preserved across pod recreation. The Jupyter server started new kernel processes upon restore, losing the old state.

### Attempt 2: IPC Transport (Initial Attempt)
*   **Approach**: Configure Jupyter to use Unix domain sockets (IPC) for kernel communication, so that socket files are stored in the shared `/tmp` directory (restored from container filesystem snapshot).
    *   Configured `c.KernelManager.transport = 'ipc'` via ConfigMap mounted to `/etc/jupyter/jupyter_server_config.py` using `subPath`.
*   **Result**: Pod connection files correctly showed IPC sockets, but the container crashed during/after checkpoint.
    *   **Error**: `failed to create containerd task: failed to start shim: can't find shim for sandbox...`
    *   **Consequence**: The pod got stuck in `Terminating`. Force-deleting it caused a network interface leak on the node (`container veth name ... already exists`), blocking subsequent pods.
*   **Resolution for Leak**: Deleted and recreated the workspace with a new name/UID to generate a different pod name and bypass the leaked interface.

### Attempt 3: IPC + Replay Protection Analysis (Multi-Cycle Failures)
*   **Approach**: Re-enabled IPC and implemented a multi-cycle test script.
*   **Result**: Succeeded once, but subsequent runs/cycles hung.
*   **Findings (from Jupyter logs)**:
    ```
    ValueError: Duplicate Signature: b'...'
    ValueError: DELIM not in msg_list
    ```
    *   **Reason**: Jupyter's ZMQ session has replay protection. If the client sends a message with the same `msg_id` and `session` ID (which the test script reused) after a restore, the restored kernel rejects it as a duplicate because the signature history was restored with the memory state.

### Attempt 4: IPC + Unique IDs + Socket Settle Delay
*   **Approach**:
    *   Modified test script to use unique UUIDs for `msg_id` and `session` per request.
    *   Added a 5-second sleep before triggering pause to allow active sockets to close cleanly on the server side before checkpoint.
*   **Result**: Succeeded for Cycle 1, but hung in Cycle 2.
    *   **Observation**: Jupyter Server was NOT spinning CPU (idle), but connection timed out. ZMQ IPC socket directly to the kernel also timed out.
    *   **Reason**: The restored IPC sockets became unresponsive after the first use post-restore.

### Attempt 5: IPC + Unique IDs + Sleep + PollSelector (Final Solution)
*   **Approach**:
    *   Identified that Tornado (used by Jupyter) defaults to `epoll` on Linux. `epoll` interest lists are stored in the kernel and are notoriously difficult to restore cleanly in sandboxed environments like gVisor, often leading to hangs or CPU spinning (tight loops on invalid FDs).
    *   Forced Jupyter Server to use `PollSelector` instead of `EpollSelector` by adding the following to `jupyter_server_config.py`:
        ```python
        import asyncio
        import selectors
        class PollEventLoopPolicy(asyncio.DefaultEventLoopPolicy):
            def _loop_factory(self):
                return asyncio.SelectorEventLoop(selectors.PollSelector())
        asyncio.set_event_loop_policy(PollEventLoopPolicy())
        ```
*   **Result**: **SUCCESS**. Ran 3 consecutive pause/resume cycles on a single kernel. State was preserved (variables incremented correctly across restarts), and Jupyter server remained healthy with low CPU usage.

---

## Final Architecture & Configuration

1.  **IPC Enabled**: ConfigMap `jupyter-ipc-config` mounted to `/etc/jupyter/jupyter_server_config.py` forces `c.KernelManager.transport = 'ipc'`.
2.  **PollSelector Forced**: The same config file overrides the `asyncio` event loop policy to use `PollSelector`, avoiding gVisor `epoll` restore bugs.
3.  **Unique Message IDs**: Client applications must use unique session/message IDs to prevent replay protection from rejecting restored states.
4.  **Graceful Pause**: A small delay before checkpointing helps ensure connections are closed cleanly.
