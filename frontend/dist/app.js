const state = { tasks: [], activeView: "download", destinationMode: "downloads", directoryHandle: null, directoryPath: "browser-downloads", browserDownloads: new Set(), savingFiles: false, auth: null };

const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => [...document.querySelectorAll(selector)];
const esc = (value = "") => String(value).replace(/[&<>'"]/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;" })[char]);

async function api(path, options) {
  const response = await fetch(path, options);
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `请求失败 (${response.status})`);
  return data;
}

function navigate(view) {
  state.activeView = view;
  $$(".view").forEach((node) => node.classList.toggle("active", node.id === `view-${view}`));
  $$(".nav-item").forEach((node) => node.classList.toggle("active", node.dataset.view === view));
  $(".sidebar").classList.remove("open");
  if (view === "queue") loadTasks();
  if (view === "system") loadSystem();
}

function statusText(task) {
  if (task.status === "running") return task.message || "下载中 (0%)";
  if (task.status === "paused") return task.message || "已暂停";
  return ({ queued: "等待中", canceling: "正在取消", canceled: "已取消", completed: "已完成", failed: "失败" })[task.status] || task.status;
}

function formatBytes(bytes) {
  if (!Number.isFinite(bytes) || bytes < 1) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / (1024 ** index)).toFixed(index ? 1 : 0)} ${units[index]}`;
}

const embeddingSupport = {
  none: { cover: true },
  flac: { cover: true },
  mp3: { cover: true },
  opus: { cover: false },
  wav: { cover: false }
};

function syncEmbeddingFormats() {
  const formatSelect = $("#convert-format");
  const coverInput = $("#embed-cover");
  const selectedSupport = embeddingSupport[formatSelect.value] || { cover: true };

  if (!selectedSupport.cover) coverInput.checked = false;
  coverInput.disabled = !selectedSupport.cover;
  coverInput.closest("label").classList.toggle("disabled", coverInput.disabled);

  [...formatSelect.options].forEach((option) => {
    const support = embeddingSupport[option.value] || { cover: true };
    option.disabled = coverInput.checked && !support.cover;
  });
}

function renderTasks() {
  $("#queue-count").textContent = state.tasks.filter((task) => ["queued", "running", "paused", "canceling"].includes(task.status)).length;
  const container = $("#task-list");
  if (!state.tasks.length) {
    container.innerHTML = '<div class="empty-state"><span>♪</span><h3>还没有下载任务</h3><p>从“新建下载”添加 Apple Music 链接。</p></div>';
    syncActiveTaskControl();
    return;
  }
  container.innerHTML = state.tasks.map((task) => {
    const firstURL = task.request.urls[0] || "Apple Music 下载";
    const extra = task.request.urls.length > 1 ? ` +${task.request.urls.length - 1}` : "";
    const title = task.title || `${firstURL}${extra}`;
    const trackPosition = task.trackTotal ? `${task.trackNumber}/${task.trackTotal}` : "";
    const details = [task.artist, task.collection, trackPosition, (task.request.quality || "alac").toUpperCase(), new Date(task.createdAt).toLocaleString()].filter(Boolean);
    const symbol = task.status === "completed" ? "✓" : task.status === "failed" ? "!" : task.status === "canceled" ? "×" : task.status === "running" ? "↓" : task.status === "paused" ? "Ⅱ" : "…";
    const files = task.files || [];
    const pendingFiles = files.filter((file) => !file.delivered);
    const totalSize = pendingFiles.reduce((sum, file) => sum + (Number(file.size) || 0), 0);
    const delivery = task.status === "completed" && pendingFiles.length
      ? `<p class="delivery-state">${state.destinationMode === "downloads" ? "正在交给浏览器下载" : "正在保存"} · ${pendingFiles.length} 个文件 · ${formatBytes(totalSize)}</p>`
      : "";
    return `<article class="task-card" data-task-id="${esc(task.id)}">
      <div class="task-head">
        <span class="task-status ${esc(task.status)}">${symbol}</span>
        <div class="task-title"><strong title="${esc(firstURL)}">${esc(title)}</strong><small>${details.map(esc).join(" · ")}</small></div>
        <span class="task-state-label">${esc(statusText(task))}</span>
      </div>
      <div class="progress"><span style="width:${Number(task.progress) || 0}%"></span></div>
      ${task.message && task.status !== "running" ? `<p class="task-message">${esc(task.message)}</p>` : ""}
      ${delivery}
    </article>`;
  }).join("");
  syncActiveTaskControl();
}

function syncActiveTaskControl() {
  const toggleButton = $("#toggle-active-task");
  const cancelButton = $("#cancel-all-tasks");
  const activeTask = state.tasks.find((item) => ["running", "paused", "canceling"].includes(item.status));
  const cancelableTasks = state.tasks.filter((item) => ["queued", "running", "paused", "canceling"].includes(item.status));
  toggleButton.hidden = !activeTask || activeTask.status === "canceling";
  cancelButton.hidden = cancelableTasks.length === 0;
  cancelButton.disabled = cancelableTasks.length > 0 && cancelableTasks.every((item) => item.status === "canceling");
  cancelButton.dataset.taskCount = String(cancelableTasks.length);
  if (!activeTask) {
    delete toggleButton.dataset.taskId;
    delete toggleButton.dataset.taskAction;
  } else {
    toggleButton.dataset.taskId = activeTask.id;
    toggleButton.dataset.taskAction = activeTask.status === "paused" ? "resume" : "pause";
    toggleButton.textContent = activeTask.status === "paused" ? "继续" : "暂停";
  }
  cancelButton.textContent = cancelButton.disabled ? "正在取消" : "取消全部";
}

function taskLayoutChanged(previous, next) {
  if (previous.length !== next.length) return true;
  return next.some((task, index) => {
    const before = previous[index];
    if (!before || before.id !== task.id || before.status !== task.status) return true;
    const beforeFiles = (before.files || []).map((file) => `${file.id}:${file.delivered}`).join("|");
    const nextFiles = (task.files || []).map((file) => `${file.id}:${file.delivered}`).join("|");
    return beforeFiles !== nextFiles;
  });
}

function patchTaskProgress() {
  for (const task of state.tasks) {
    const card = $(`#task-list [data-task-id="${task.id}"]`);
    if (!card) continue;
    const progress = card.querySelector(".progress span");
    const label = card.querySelector(".task-state-label");
    if (progress) progress.style.width = `${Number(task.progress) || 0}%`;
    if (label) label.textContent = statusText(task);
  }
  syncActiveTaskControl();
}

let taskLoadPromise = null;
async function loadTasks() {
  if (taskLoadPromise) return taskLoadPromise;
  taskLoadPromise = (async () => {
    try {
      const tasks = await api("/api/tasks");
      const needsRender = taskLayoutChanged(state.tasks, tasks);
      state.tasks = tasks;
      if (needsRender) renderTasks();
      else patchTaskProgress();
      savePendingFiles();
    } catch (_) {}
  })();
  try {
    await taskLoadPromise;
  } finally {
    taskLoadPromise = null;
  }
}

async function pollTasks() {
  await loadTasks();
  const hasRunningTask = state.tasks.some((task) => task.status === "running");
  window.setTimeout(pollTasks, !document.hidden && hasRunningTask ? 50 : 1000);
}

async function chooseDownloadDirectory() {
  const message = $("#form-message");
  if (!("showDirectoryPicker" in window)) {
    message.textContent = "当前浏览器不支持目录写入，请使用本机 Chrome、Edge 或打包后的桌面应用。";
    message.className = "form-message show error";
    return false;
  }
  try {
    state.directoryHandle = await window.showDirectoryPicker({
      id: "apple-music-download-directory",
      mode: "readwrite",
      startIn: "downloads"
    });
    const exactPath = [state.directoryHandle.path, state.directoryHandle.fullPath]
      .find((value) => typeof value === "string" && value.trim());
    const directoryPath = exactPath || `…/${state.directoryHandle.name}`;
    state.directoryPath = directoryPath;
    $("#directory-name").textContent = directoryPath;
    message.textContent = `已选择“${directoryPath}”，完成的文件将自动保存。`;
    message.className = "form-message show";
    await savePendingFiles();
    return true;
  } catch (error) {
    if (error.name !== "AbortError") {
      const protectedDirectory = error.name === "NotAllowedError" || /system files|sensitive|not allowed/i.test(error.message);
      message.textContent = protectedDirectory
        ? "浏览器不允许直接使用这个受保护目录。请在 Downloads 中新建 AppleMusic 子目录，然后选择该子目录。"
        : error.message;
      message.className = "form-message show error";
    }
    return false;
  }
}

async function directoryFor(relativePath) {
  const parts = relativePath.split("/").filter(Boolean);
  parts.pop();
  let directory = state.directoryHandle;
  for (const part of parts) directory = await directory.getDirectoryHandle(part, { create: true });
  return directory;
}

async function saveTaskFile(task, file) {
  const response = await fetch(`/api/tasks/${encodeURIComponent(task.id)}/files/${encodeURIComponent(file.id)}`);
  if (!response.ok || !response.body) throw new Error(`读取 ${file.name} 失败 (${response.status})`);
  const directory = await directoryFor(file.relativePath);
  const handle = await directory.getFileHandle(file.name, { create: true });
  const writable = await handle.createWritable();
  await response.body.pipeTo(writable);
  await api(`/api/tasks/${encodeURIComponent(task.id)}/files/${encodeURIComponent(file.id)}`, { method: "DELETE" });
}

function downloadTaskFileWithBrowser(task, file) {
  const key = `${task.id}:${file.id}`;
  if (state.browserDownloads.has(key)) return;
  state.browserDownloads.add(key);
  const link = document.createElement("a");
  link.href = `/api/tasks/${encodeURIComponent(task.id)}/files/${encodeURIComponent(file.id)}?delivery=browser`;
  link.download = file.name;
  link.hidden = true;
  document.body.appendChild(link);
  link.click();
  link.remove();
  // Normally the next queue refresh removes the delivered file. Allow a
  // retry later if the request never reached the backend.
  window.setTimeout(() => state.browserDownloads.delete(key), 5 * 60 * 1000);
}

async function savePendingFiles() {
  if (state.savingFiles) return;
  if (state.destinationMode === "downloads") {
    for (const task of state.tasks.filter((item) => item.status === "completed")) {
      for (const file of (task.files || []).filter((item) => !item.delivered)) {
        downloadTaskFileWithBrowser(task, file);
      }
    }
    return;
  }
  if (!state.directoryHandle) return;
  state.savingFiles = true;
  const message = $("#form-message");
  try {
    for (const task of state.tasks.filter((item) => item.status === "completed")) {
      for (const file of (task.files || []).filter((item) => !item.delivered)) {
        await saveTaskFile(task, file);
      }
    }
    state.tasks = await api("/api/tasks");
    renderTasks();
  } catch (error) {
    message.textContent = `保存文件失败：${error.message}。容器暂存文件仍保留，可重新选择目录后重试。`;
    message.className = "form-message show error";
  } finally {
    state.savingFiles = false;
  }
}

async function loadHealth() {
  const dot = $("#health-dot");
  try {
    const health = await api("/api/health");
    dot.className = "online";
    $("#health-text").textContent = "后端在线";
    $("#runtime-detail").textContent = `${health.os}/${health.arch}`;
  } catch (_) {
    dot.className = "offline";
    $("#health-text").textContent = "后端离线";
  }
}

async function loadSystem() {
  await loadHealth();
  await loadWrapperAuth();
  const grid = $("#dependency-grid");
  grid.innerHTML = '<div class="dependency"><strong>正在检测…</strong></div>';
  try {
    const dependencies = await api("/api/dependencies");
    grid.innerHTML = Object.entries(dependencies).map(([name, item]) => `<div class="dependency ${item.available ? "available" : ""}"><div class="dependency-top"><strong>${esc(name)}</strong><i></i></div><p title="${esc(item.path || "未找到")}">${esc(item.path || "未找到")}</p></div>`).join("");
  } catch (error) {
    grid.innerHTML = `<div class="dependency"><strong>${esc(error.message)}</strong></div>`;
  }
}

async function loadWrapperAuth() {
  const dot = $("#wrapper-auth-dot");
  const detail = $("#wrapper-auth-detail");
  try {
    const auth = await api("/api/wrapper/auth");
    state.auth = auth;
    dot.className = `auth-dot ${auth.state === "authenticated" || auth.state === "disabled" ? "ready" : ["starting", "signing-in", "awaiting-2fa", "verifying-2fa"].includes(auth.state) ? "pending" : ""}`;
    detail.textContent = auth.message;
    renderLogin(auth);
  } catch (error) {
    dot.className = "auth-dot";
    detail.textContent = error.message;
    renderLogin({ state: "failed", authRequired: true, requiresCredentials: true, message: `无法读取登录状态：${error.message}`, recentEvents: [] });
  }
}

function renderLogin(auth) {
  const modal = $("#login-modal");
  const accountForm = $("#login-form");
  const twoFAForm = $("#login-2fa-form");
  const feedback = $(".login-feedback");
  const message = $("#login-message");
  const openButton = $("#open-login");
  const logoutButton = $("#logout-wrapper");
  const waiting = ["starting", "signing-in", "verifying-2fa", "logging-out"].includes(auth.state);
  const showTwoFA = Boolean(auth.requiresTwoFA);

  if (!auth.authRequired) {
    modal.hidden = true;
    document.body.classList.remove("auth-locked");
    openButton.hidden = true;
  } else {
    modal.hidden = false;
    document.body.classList.add("auth-locked");
    openButton.hidden = false;
  }

  logoutButton.hidden = auth.state !== "authenticated";
  logoutButton.disabled = auth.state === "logging-out";

  accountForm.hidden = !auth.requiresCredentials;
  twoFAForm.hidden = !showTwoFA;
  accountForm.querySelector("button[type=submit]").disabled = waiting;
  twoFAForm.querySelector("button[type=submit]").disabled = auth.state === "verifying-2fa";
  $("#login-step-account").className = showTwoFA ? "done" : "active";
  $("#login-step-2fa").className = showTwoFA ? "active" : "";
  const feedbackState = auth.state === "failed" || auth.state === "unavailable"
    ? "error"
    : auth.state === "authenticated"
      ? "ready"
      : waiting
        ? "pending"
        : "idle";
  feedback.className = `login-feedback ${feedbackState}`;
  message.textContent = auth.message || "正在等待 Wrapper…";
  $("#login-events").innerHTML = (auth.recentEvents || []).map((item) => `<li>${esc(item)}</li>`).join("");
}

$("#login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.currentTarget.querySelector("button[type=submit]");
  const username = $("#login-username").value.trim();
  const passwordInput = $("#login-password");
  const password = passwordInput.value;
  button.disabled = true;
  try {
    const auth = await api("/api/wrapper/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password })
    });
    state.auth = auth;
    renderLogin(auth);
  } catch (error) {
    renderLogin({ state: "failed", authRequired: true, requiresCredentials: true, message: error.message, recentEvents: state.auth?.recentEvents || [] });
  } finally {
    passwordInput.value = "";
    button.disabled = false;
  }
});

$("#login-2fa-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const input = $("#login-2fa-code");
  const button = event.currentTarget.querySelector("button[type=submit]");
  button.disabled = true;
  try {
    const auth = await api("/api/wrapper/2fa", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code: input.value.trim() })
    });
    input.value = "";
    state.auth = auth;
    renderLogin(auth);
  } catch (error) {
    renderLogin({ ...(state.auth || {}), state: "awaiting-2fa", authRequired: true, requiresTwoFA: true, message: error.message });
  } finally {
    button.disabled = false;
  }
});

$("#download-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.currentTarget.querySelector("button[type=submit]");
  const message = $("#form-message");
  const urls = $("#urls").value.split(/\n+/).map((url) => url.trim()).filter(Boolean);
  const quality = $('input[name="quality"]:checked').value;
  message.className = "form-message";
  button.disabled = true;
  try {
    if (state.destinationMode === "other" && (!state.directoryHandle || !state.directoryPath)) {
      throw new Error("请先选择下载目录，再加入下载队列。");
    }
    const result = await api("/api/tasks", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        urls,
        quality,
        outputPath: state.destinationMode === "downloads" ? "browser-downloads" : state.directoryPath,
        convertFormat: $("#convert-format").value,
        embedCover: $("#embed-cover").checked
      })
    });
    const tasks = result.tasks || [];
    $("#urls").value = "";
    message.textContent = tasks.length > 1
      ? `已解析并加入 ${tasks.length} 个曲目任务。`
      : `任务 ${(tasks[0]?.id || "").slice(0, 8)} 已加入队列。`;
    message.className = "form-message show";
    await loadTasks();
    setTimeout(() => navigate("queue"), 450);
  } catch (error) {
    message.textContent = error.message;
    message.className = "form-message show error";
  } finally {
    button.disabled = false;
  }
});

$$('.quality input').forEach((input) => input.addEventListener("change", () => {
  $$(".quality").forEach((label) => label.classList.toggle("active", label.contains($('input[name="quality"]:checked'))));
}));

$("#embed-cover").addEventListener("change", syncEmbeddingFormats);
$("#convert-format").addEventListener("change", syncEmbeddingFormats);
syncEmbeddingFormats();

async function refreshWithAnimation(button, refresh) {
  button.disabled = true;
  button.classList.add("loading");
  button.setAttribute("aria-busy", "true");
  try {
    await new Promise((resolve) => setTimeout(resolve, 100));
    await refresh();
  } finally {
    button.disabled = false;
    button.classList.remove("loading");
    button.removeAttribute("aria-busy");
  }
}

$$('.nav-item').forEach((button) => button.addEventListener("click", () => navigate(button.dataset.view)));
$("#mobile-nav-toggle").addEventListener("click", () => $(".sidebar").classList.toggle("open"));
$("#refresh-tasks").addEventListener("click", (event) => refreshWithAnimation(event.currentTarget, loadTasks));
$("#toggle-active-task").addEventListener("click", async (event) => {
  const button = event.currentTarget;
  if (!button.dataset.taskId || !button.dataset.taskAction) return;
  button.disabled = true;
  try {
    await api(`/api/tasks/${encodeURIComponent(button.dataset.taskId)}/${button.dataset.taskAction}`, { method: "POST" });
    await loadTasks();
  } catch (error) {
    window.alert(error.message);
  } finally {
    button.disabled = false;
  }
});
$("#cancel-all-tasks").addEventListener("click", async (event) => {
  const button = event.currentTarget;
  const taskCount = Number(button.dataset.taskCount) || 0;
  if (!taskCount) return;
  if (!window.confirm(`确定取消全部 ${taskCount} 个未完成任务？已下载的临时数据会被删除。`)) return;
  button.disabled = true;
  try {
    await api("/api/tasks/cancel-all", { method: "POST" });
    await loadTasks();
  } catch (error) {
    window.alert(error.message);
  } finally {
    button.disabled = false;
  }
});
$("#refresh-system").addEventListener("click", (event) => refreshWithAnimation(event.currentTarget, loadSystem));
$("#download-destination").addEventListener("change", async (event) => {
  const selector = event.currentTarget;
  if (selector.value === "downloads") {
    state.destinationMode = "downloads";
    state.directoryHandle = null;
    state.directoryPath = "browser-downloads";
    $("#directory-name").textContent = "由浏览器的下载设置决定";
    savePendingFiles();
    return;
  }

  state.destinationMode = "other";
  if (!await chooseDownloadDirectory()) {
    selector.value = "downloads";
    state.destinationMode = "downloads";
    state.directoryHandle = null;
    state.directoryPath = "browser-downloads";
    $("#directory-name").textContent = "由浏览器的下载设置决定";
  }
});
$("#open-login").addEventListener("click", loadWrapperAuth);
$("#logout-wrapper").addEventListener("click", async () => {
  if (!window.confirm("确定退出 Wrapper？容器内的 Apple 登录会话将被清除，下载队列不会受影响。")) return;
  const button = $("#logout-wrapper");
  button.disabled = true;
  try {
    const auth = await api("/api/wrapper/logout", { method: "POST" });
    state.auth = auth;
    $("#wrapper-auth-dot").className = "auth-dot";
    $("#wrapper-auth-detail").textContent = auth.message;
    renderLogin(auth);
  } catch (error) {
    $("#wrapper-auth-detail").textContent = `退出失败：${error.message}`;
  } finally {
    button.disabled = false;
  }
});

const savedTheme = localStorage.getItem("amdl-theme") || "dark";
document.documentElement.dataset.theme = savedTheme;
$("#theme-toggle").addEventListener("click", () => {
  const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = next;
  localStorage.setItem("amdl-theme", next);
});

loadHealth();
pollTasks();
loadWrapperAuth();
setInterval(loadWrapperAuth, 900);
