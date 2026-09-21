const form = document.querySelector("#convert-form");
const setupGuide = document.querySelector("#settings-modal .setup-guide");
const setupSummary = setupGuide.querySelector("summary");
const coneArt = document.querySelector("#cone-art");
const coneImage = document.querySelector("#cone-image");
const input = document.querySelector("#pack-input");
const apiKeyInput = document.querySelector("#api-key-input");
const userIDInput = document.querySelector("#user-id-input");
const secretToggle = document.querySelector("#secret-toggle");
const rememberCredentials = document.querySelector("#remember-credentials");
const dropZone = document.querySelector("#drop-zone");
const selectedFile = document.querySelector("#selected-file");
const fileName = document.querySelector("#file-name");
const fileSize = document.querySelector("#file-size");
const removeFile = document.querySelector("#remove-file");
const convertButton = document.querySelector("#convert-button");
const progressPanel = document.querySelector("#progress-panel");
const progressTrack = document.querySelector("#progress-track");
const progressBar = document.querySelector("#progress-bar");
const statusLabel = document.querySelector("#status-label");
const statusDetail = document.querySelector("#status-detail");
const statusPercent = document.querySelector("#status-percent");
const statusError = document.querySelector("#status-error");
const activityLog = document.querySelector("#activity-log");
const resultModal = document.querySelector("#result-modal");
const resultBackdrop = document.querySelector("#result-backdrop");
const copyButton = document.querySelector("#copy-button");
const closeResultButton = document.querySelector("#close-result-button");
const previewModal = document.querySelector("#preview-modal");
const previewBackdrop = document.querySelector("#preview-backdrop");
const previewCloseButton = document.querySelector("#preview-close-button");
const previewImage = document.querySelector("#preview-image");
const previewTitle = document.querySelector("#preview-title");
const previewMeta = document.querySelector("#preview-meta");
const skyModal = document.querySelector("#sky-modal");
const skyModalBackdrop = document.querySelector("#sky-modal-backdrop");
const skyOptionsContainer = document.querySelector("#sky-options");
const skyAllExtrasButton = document.querySelector("#sky-all-extras-button");
const skySkipButton = document.querySelector("#sky-skip-button");
const skyCancelButton = document.querySelector("#sky-cancel-button");
const skyPortButton = document.querySelector("#sky-port-button");

let currentFile = null;
let resultJSON = "";
let audioContext = null;
let playedCompleteSound = false;

const coneImages = {
  idle: "/cone.png",
  accepted: "/cone_accepted.png",
  error: "/cone_error.png",
};

for (const source of Object.values(coneImages)) {
  const image = new Image();
  image.src = source;
}

const storageKeys = {
  remember: "cone.rememberCredentials",
  apiKey: "cone.robloxApiKey",
  userID: "cone.robloxUserId",
};

function tone(frequency, start, duration, volume = 0.025, type = "sine") {
  const oscillator = audioContext.createOscillator();
  const gain = audioContext.createGain();
  oscillator.type = type;
  oscillator.frequency.setValueAtTime(frequency, start);
  gain.gain.setValueAtTime(0.0001, start);
  gain.gain.exponentialRampToValueAtTime(volume, start + 0.012);
  gain.gain.exponentialRampToValueAtTime(0.0001, start + duration);
  oscillator.connect(gain);
  gain.connect(audioContext.destination);
  oscillator.start(start);
  oscillator.stop(start + duration + 0.02);
}

function playSound(name) {
  try {
    audioContext ||= new AudioContext();
    if (audioContext.state === "suspended") audioContext.resume();
    const now = audioContext.currentTime + 0.01;
    const sounds = {
      tap: [[510, 0, 0.045]],
      select: [[440, 0, 0.06], [660, 0.055, 0.08]],
      start: [[300, 0, 0.07], [420, 0.07, 0.08]],
      complete: [[523, 0, 0.08], [659, 0.07, 0.08], [784, 0.14, 0.13]],
      error: [[210, 0, 0.1], [150, 0.09, 0.14]],
      copy: [[720, 0, 0.07]],
    };
    for (const [frequency, delay, duration] of sounds[name] || []) {
      tone(frequency, now + delay, duration, name === "error" ? 0.018 : 0.022, "triangle");
    }
  } catch {
    // Sound is optional; conversion must work when Web Audio is unavailable.
  }
}

function setConeState(state) {
  const source = coneImages[state] || coneImages.idle;
  if (coneArt.dataset.state === state && coneImage.getAttribute("src") === source) return;
  coneArt.dataset.state = state;
  coneImage.src = source;
  coneArt.classList.remove("is-changing");
  void coneArt.offsetWidth;
  coneArt.classList.add("is-changing");
}

function createClickSpark(event) {
  if (event.button !== 0 || window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;
  const spark = document.createElement("span");
  spark.className = "click-spark";
  spark.style.left = `${event.clientX}px`;
  spark.style.top = `${event.clientY}px`;
  for (let index = 0; index < 8; index += 1) {
    const ray = document.createElement("span");
    ray.className = "click-spark-ray";
    ray.style.setProperty("--spark-angle", `${index * 45}deg`);
    spark.append(ray);
  }
  document.body.append(spark);
  window.setTimeout(() => spark.remove(), 480);
}

function readSavedCredentials() {
  try {
    if (localStorage.getItem(storageKeys.remember) === "false") {
      rememberCredentials.checked = false;
      if (window.matchMedia("(max-width: 480px)").matches) setupGuide.open = false;
      return;
    }
    rememberCredentials.checked = true;
    apiKeyInput.value = localStorage.getItem(storageKeys.apiKey) || "";
    userIDInput.value = localStorage.getItem(storageKeys.userID) || "";
    if ((apiKeyInput.value && userIDInput.value) || window.matchMedia("(max-width: 480px)").matches) {
      setupGuide.open = false;
    }
  } catch {
    rememberCredentials.checked = false;
  }
}

function saveCredentials() {
  try {
    if (!rememberCredentials.checked) {
      localStorage.setItem(storageKeys.remember, "false");
      localStorage.removeItem(storageKeys.apiKey);
      localStorage.removeItem(storageKeys.userID);
      return;
    }
    localStorage.setItem(storageKeys.remember, "true");
    localStorage.setItem(storageKeys.apiKey, apiKeyInput.value);
    localStorage.setItem(storageKeys.userID, userIDInput.value);
  } catch {
    rememberCredentials.checked = false;
  }
}

function formatBytes(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB"];
  let value = bytes / 1024;
  let unit = units[0];
  for (let index = 1; value >= 1024 && index < units.length; index += 1) {
    value /= 1024;
    unit = units[index];
  }
  return `${value.toFixed(value >= 10 ? 1 : 2)} ${unit}`;
}

function setFile(file) {
  if (!file) {
    currentFile = null;
    input.value = "";
    selectedFile.hidden = true;
    resetOutput();
    updateConvertState();
    return;
  }
  if (!file.name.toLowerCase().endsWith(".zip")) {
    showError("Choose a Minecraft texture-pack ZIP file.");
    return;
  }
  currentFile = file;
  fileName.textContent = file.name;
  fileSize.textContent = formatBytes(file.size);
  selectedFile.hidden = false;
  updateConvertState();
  resetOutput();
  playSound("select");
}

function updateConvertState() {
  convertButton.disabled = !currentFile || !apiKeyInput.value.trim() || !userIDInput.value.trim();
}

function resetOutput() {
  progressPanel.hidden = true;
  closeResultModal(false);
  statusError.hidden = true;
  activityLog.replaceChildren();
  resultJSON = "";
  playedCompleteSound = false;
  copyButton.textContent = "Copy JSON";
  setConeState("idle");
}

function appendLog(message, isError = false) {
  if (!message) return;
  const line = document.createElement("p");
  line.textContent = message;
  line.classList.toggle("is-error", isError);
  activityLog.append(line);
  activityLog.scrollTop = activityLog.scrollHeight;
}

function updateProgress(percent, label, detail) {
  const safePercent = Math.max(0, Math.min(100, Math.round(percent)));
  progressPanel.hidden = false;
  progressBar.style.width = `${safePercent}%`;
  progressTrack.setAttribute("aria-valuenow", String(safePercent));
  statusPercent.textContent = `${safePercent}%`;
  statusLabel.textContent = label;
  statusDetail.textContent = detail;
}

function showError(message) {
  progressPanel.hidden = false;
  closeResultModal(false);
  statusLabel.textContent = "Couldn't convert pack";
  statusDetail.textContent = "Check the file and try again.";
  statusError.textContent = message;
  statusError.hidden = true;
  appendLog(message, true);
  setConeState("error");
  playSound("error");
}

function handleProgress(progress) {
  statusError.hidden = true;
  const message = progress.message || progress.name || "Working…";
  if (progress.stage === "receiving") {
    updateProgress(4, "Porting...", message);
    appendLog(message);
    return;
  }
  if (progress.stage === "preparing") {
    updateProgress(12, "Porting...", message);
    appendLog(message);
    return;
  }
  if (progress.stage === "uploading") {
    const ratio = progress.total ? progress.completed / progress.total : 0;
    updateProgress(12 + ratio * 83, "Porting...", message);
    appendLog(progress.error ? `${message}: ${progress.error}` : message, Boolean(progress.error));
    return;
  }
  if (progress.stage === "complete") {
    updateProgress(100, "Ported", message);
    appendLog(message);
    if (!playedCompleteSound) {
      playedCompleteSound = true;
      playSound("complete");
    }
    return;
  }
  if (progress.stage === "notifying") {
    updateProgress(100, "Ported", message);
    appendLog(progress.error ? `${message}: ${progress.error}` : message, Boolean(progress.error));
  }
}

function handleResult(event) {
  resultJSON = JSON.stringify(event.result);
  resultModal.hidden = false;
  document.body.classList.add("modal-open");
  setConeState("accepted");
  window.requestAnimationFrame(() => copyButton.focus({ preventScroll: true }));
}

function closeResultModal(withSound = true) {
  if (resultModal.hidden) return;
  if (withSound) playSound("tap");
  resultModal.hidden = true;
  document.body.classList.remove("modal-open");
  convertButton.focus({ preventScroll: true });
}

async function copyJSON() {
  if (!resultJSON) return;
  try {
    await navigator.clipboard.writeText(resultJSON);
  } catch {
    const field = document.createElement("textarea");
    field.value = resultJSON;
    field.setAttribute("readonly", "");
    field.className = "copy-fallback";
    document.body.append(field);
    field.select();
    document.execCommand("copy");
    field.remove();
  }
  copyButton.textContent = "Copied";
  playSound("copy");
  window.setTimeout(() => {
    copyButton.textContent = "Copy JSON";
  }, 1600);
}

async function consumeEvents(response) {
  if (!response.ok) {
    const responseText = (await response.text()).trim();
    const contentType = response.headers.get("content-type") || "";
    const isHTML = contentType.includes("text/html") || /(?:<!?doctype\s+html|<html)/i.test(responseText);
    if (isHTML) {
      throw new Error(`Cone server is temporarily unavailable (${response.status}). Try again shortly.`);
    }
    throw new Error(responseText || `Server returned ${response.status}`);
  }
  if (!response.body) {
    throw new Error("This browser cannot read conversion progress.");
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffered = "";
  while (true) {
    const { value, done } = await reader.read();
    buffered += decoder.decode(value || new Uint8Array(), { stream: !done });
    const lines = buffered.split("\n");
    buffered = lines.pop() || "";
    for (const line of lines) {
      if (!line.trim()) continue;
      const event = JSON.parse(line);
      if (event.type === "progress") handleProgress(event.progress);
      if (event.type === "result") handleResult(event);
      if (event.type === "error") throw new Error(event.message || "Conversion failed.");
    }
    if (done) break;
  }
}

input.addEventListener("change", () => setFile(input.files[0]));
removeFile.addEventListener("click", () => {
  playSound("tap");
  setFile(null);
});
copyButton.addEventListener("click", copyJSON);
closeResultButton.addEventListener("click", () => closeResultModal());
resultBackdrop.addEventListener("click", () => closeResultModal());
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && !resultModal.hidden) closeResultModal();
});
secretToggle.addEventListener("click", () => {
  playSound("tap");
  const showing = apiKeyInput.type === "text";
  apiKeyInput.type = showing ? "password" : "text";
  secretToggle.textContent = showing ? "Show" : "Hide";
  secretToggle.setAttribute("aria-label", showing ? "Show API key" : "Hide API key");
  secretToggle.setAttribute("aria-pressed", String(!showing));
  apiKeyInput.focus({ preventScroll: true });
});
setupSummary.addEventListener("click", () => playSound("tap"));
apiKeyInput.addEventListener("input", () => {
  saveCredentials();
  updateConvertState();
});
userIDInput.addEventListener("input", () => {
  saveCredentials();
  updateConvertState();
});
rememberCredentials.addEventListener("change", saveCredentials);

readSavedCredentials();
updateConvertState();

// ----------------------------------------------------------- settings modal
// Credentials moved out of the main flow and into a gear-icon modal so the
// home screen only shows the one thing most visits need: the drop zone.
const settingsModal = document.querySelector("#settings-modal");
const settingsBackdrop = document.querySelector("#settings-backdrop");
const settingsOpenButton = document.querySelector("#settings-open-button");
const settingsDoneButton = document.querySelector("#settings-done-button");

function openSettingsModal() {
  settingsModal.hidden = false;
  document.body.classList.add("modal-open");
  window.setTimeout(() => userIDInput.focus({ preventScroll: true }), 0);
}

function closeSettingsModal() {
  settingsModal.hidden = true;
  if (previewModal.hidden && skyModal.hidden && resultModal.hidden) {
    document.body.classList.remove("modal-open");
  }
}

const settingsIcon = settingsOpenButton.querySelector(".settings-icon");
settingsOpenButton.addEventListener("click", () => {
  openSettingsModal();
  settingsIcon.classList.remove("is-spinning");
  // Force a reflow so re-adding the class retriggers the animation even if
  // it's already mid-spin from a rapid double-click.
  void settingsIcon.offsetWidth;
  settingsIcon.classList.add("is-spinning");
});
settingsIcon.addEventListener("animationend", () => settingsIcon.classList.remove("is-spinning"));
settingsBackdrop.addEventListener("click", closeSettingsModal);
settingsDoneButton.addEventListener("click", closeSettingsModal);

// First-time visitors (nothing saved yet) get the settings modal opened for
// them automatically, since otherwise there's no obvious reason the Port
// button would be disabled.
if (!apiKeyInput.value.trim() || !userIDInput.value.trim()) {
  openSettingsModal();
}

for (const eventName of ["dragenter", "dragover"]) {
  dropZone.addEventListener(eventName, (event) => {
    event.preventDefault();
    dropZone.classList.add("is-dragging");
  });
}
for (const eventName of ["dragleave", "drop"]) {
  dropZone.addEventListener(eventName, (event) => {
    event.preventDefault();
    dropZone.classList.remove("is-dragging");
  });
}
dropZone.addEventListener("drop", (event) => setFile(event.dataTransfer.files[0]));
dropZone.addEventListener("pointermove", (event) => {
  const bounds = dropZone.getBoundingClientRect();
  dropZone.style.setProperty("--spot-x", `${event.clientX - bounds.left}px`);
  dropZone.style.setProperty("--spot-y", `${event.clientY - bounds.top}px`);
});
dropZone.addEventListener("pointerleave", () => {
  dropZone.style.removeProperty("--spot-x");
  dropZone.style.removeProperty("--spot-y");
});
document.addEventListener("pointerdown", createClickSpark);

/* ============================================================= routing === */
const navButtons = document.querySelectorAll(".nav-button");
const pageViews = document.querySelectorAll(".page-view");

function currentRoute() {
  const hash = window.location.hash.replace(/^#\/?/, "");
  return hash || "home";
}

function renderRoute() {
  const route = currentRoute();
  for (const view of pageViews) {
    view.hidden = view.id !== `view-${route}`;
  }
  for (const button of navButtons) {
    const isCurrent = button.dataset.route === route;
    if (isCurrent) button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
  }
  if (route === "library") loadLibrary();
}

window.addEventListener("hashchange", renderRoute);
renderRoute();

/* ============================================================= library === */
const historyList = document.querySelector("#history-list");
const historyState = document.querySelector("#history-state");
const historySearch = document.querySelector("#history-search");
const librarySort = document.querySelector("#library-sort");

let libraryPacks = null;
let libraryLoadFailed = false;

function formatLibraryTimestamp(iso) {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  const pad = (value) => String(value).padStart(2, "0");
  let hours = date.getHours();
  const meridiem = hours >= 12 ? "PM" : "AM";
  hours = hours % 12;
  if (hours === 0) hours = 12;
  return `${pad(date.getDate())}/${pad(date.getMonth() + 1)}/${date.getFullYear()}, ` +
    `${hours}:${pad(date.getMinutes())} ${meridiem}`;
}

async function loadLibrary() {
  if (libraryPacks !== null || libraryLoadFailed) {
    renderLibrary();
    return;
  }
  historyState.hidden = false;
  historyState.textContent = "Loading port history…";
  try {
    const response = await fetch("/api/library");
    if (!response.ok) throw new Error(`Server returned ${response.status}`);
    const data = await response.json();
    libraryPacks = Array.isArray(data.packs) ? data.packs : [];
    renderLibrary();
  } catch (error) {
    libraryLoadFailed = true;
    historyState.hidden = false;
    historyState.textContent = "Couldn't load port history. Try refreshing the page.";
  }
}

function renderLibrary() {
  if (!libraryPacks) return;
  const query = historySearch.value.trim().toLowerCase();
  let packs = libraryPacks.filter((pack) => {
    if (!query) return true;
    const haystack = `${pack.packName} ${pack.packId} ${pack.sequence}`.toLowerCase();
    return haystack.includes(query);
  });
  packs = packs.slice().sort((a, b) =>
    librarySort.value === "oldest" ? a.sequence - b.sequence : b.sequence - a.sequence);

  historyList.querySelectorAll(".history-row").forEach((row) => row.remove());
  stopPreviewCycles();

  if (packs.length === 0) {
    historyState.hidden = false;
    historyState.textContent = libraryPacks.length === 0
      ? "Nothing has been ported yet."
      : "No packs match your search.";
    return;
  }
  historyState.hidden = true;

  const fragment = document.createDocumentFragment();
  for (const pack of packs) {
    fragment.append(buildHistoryRow(pack));
  }
  historyList.append(fragment);
}

// The hotbar preview PNG is a strip of up to four item icons in a row (see
// preview.go: previewMargin/previewSlotSize/previewGap). Each slot also has
// a 4px light/dark bevel drawn right at its edge (drawHotbarSlot), so the
// crop below starts a little inside the slot to leave that bevel out of the
// thumbnail. Rather than showing the whole squashed strip in a 72x72
// thumbnail, cycle through each icon in place so the thumbnail itself
// previews everything in the pack.
const PREVIEW_SLOT_SIZE = 96;
const PREVIEW_GAP = 8;
const PREVIEW_MARGIN = 16;
const PREVIEW_SLOT_BORDER = 4;
const PREVIEW_THUMB_SIZE = 72;
const PREVIEW_CYCLE_MS = 1000;

let previewCycleIntervals = [];

function stopPreviewCycles() {
  for (const intervalId of previewCycleIntervals) window.clearInterval(intervalId);
  previewCycleIntervals = [];
}

function startPreviewCycle(button, url) {
  const probe = new Image();
  probe.addEventListener("load", () => {
    const slotCount = Math.max(1, Math.round(
      (probe.naturalWidth - PREVIEW_MARGIN * 2 + PREVIEW_GAP) / (PREVIEW_SLOT_SIZE + PREVIEW_GAP)
    ));
    const innerSlotSize = PREVIEW_SLOT_SIZE - PREVIEW_SLOT_BORDER * 2;
    const scale = PREVIEW_THUMB_SIZE / innerSlotSize;
    button.style.backgroundImage = `url("${url}")`;
    button.style.backgroundRepeat = "no-repeat";
    button.style.backgroundSize = `${probe.naturalWidth * scale}px ${probe.naturalHeight * scale}px`;
    button.style.backgroundPositionY = `${-(PREVIEW_MARGIN + PREVIEW_SLOT_BORDER) * scale}px`;

    let slotIndex = 0;
    const showSlot = () => {
      const slotX = PREVIEW_MARGIN + slotIndex * (PREVIEW_SLOT_SIZE + PREVIEW_GAP) + PREVIEW_SLOT_BORDER;
      button.style.backgroundPositionX = `${-slotX * scale}px`;
      slotIndex = (slotIndex + 1) % slotCount;
    };
    showSlot();
    if (slotCount > 1) {
      previewCycleIntervals.push(window.setInterval(showSlot, PREVIEW_CYCLE_MS));
    }
  });
  probe.addEventListener("error", () => button.remove());
  probe.src = url;
}

function buildHistoryRow(pack) {
  const row = document.createElement("div");
  row.className = "history-row";

  if (pack.hasPreview) {
    const previewURL = `/api/library/${pack.sequence}/preview.png`;
    const preview = document.createElement("button");
    preview.type = "button";
    preview.className = "history-preview";
    preview.setAttribute("aria-label", `View full preview for ${pack.packName}`);
    preview.addEventListener("click", () => openPreviewModal(pack, previewURL));
    row.append(preview);
    startPreviewCycle(preview, previewURL);
  }

  const info = document.createElement("div");
  info.className = "history-info";
  const name = document.createElement("strong");
  name.textContent = pack.packName;
  const meta = document.createElement("span");
  meta.textContent = `#${pack.sequence} · ${formatLibraryTimestamp(pack.createdAt)}`;
  info.append(name, meta);
  row.append(info);

  const actions = document.createElement("div");
  actions.className = "history-actions";
  const copyCodeButton = document.createElement("button");
  copyCodeButton.className = "chip";
  copyCodeButton.type = "button";
  copyCodeButton.textContent = "Copy json";
  copyCodeButton.addEventListener("click", () => copyLibraryCode(pack.sequence, copyCodeButton));
  actions.append(copyCodeButton);
  row.append(actions);

  return row;
}

async function copyLibraryCode(sequence, button) {
  const originalLabel = button.textContent;
  try {
    const response = await fetch(`/api/library/${sequence}/code`);
    if (!response.ok) throw new Error(`Server returned ${response.status}`);
    const code = await response.text();
    try {
      await navigator.clipboard.writeText(code);
    } catch {
      const field = document.createElement("textarea");
      field.value = code;
      field.setAttribute("readonly", "");
      field.className = "copy-fallback";
      document.body.append(field);
      field.select();
      document.execCommand("copy");
      field.remove();
    }
    playSound("copy");
    button.textContent = "Copied";
    button.classList.add("is-copied");
  } catch {
    button.textContent = "Failed";
  } finally {
    window.setTimeout(() => {
      button.textContent = originalLabel;
      button.classList.remove("is-copied");
    }, 1600);
  }
}

function openPreviewModal(pack, previewURL) {
  previewImage.src = previewURL;
  previewImage.alt = pack.packName;
  previewTitle.textContent = pack.packName;
  previewMeta.textContent = `#${pack.sequence} · ${pack.packId} · ${formatLibraryTimestamp(pack.createdAt)} · ${formatBytes(pack.sizeBytes)}`;
  previewModal.hidden = false;
  document.body.classList.add("modal-open");
  playSound("tap");
  window.requestAnimationFrame(() => previewCloseButton.focus({ preventScroll: true }));
}

function closePreviewModal() {
  if (previewModal.hidden) return;
  playSound("tap");
  previewModal.hidden = true;
  previewImage.src = "";
  document.body.classList.remove("modal-open");
}

previewCloseButton.addEventListener("click", closePreviewModal);
previewBackdrop.addEventListener("click", closePreviewModal);
document.addEventListener("keydown", (event) => {
  if (event.key !== "Escape") return;
  if (!previewModal.hidden) closePreviewModal();
  if (!skyModal.hidden) closeSkyModal();
  if (!settingsModal.hidden) closeSettingsModal();
});

historySearch.addEventListener("input", renderLibrary);
librarySort.addEventListener("change", renderLibrary);

// --------------------------------------------------------------- sky modal
// A pack that ships more than one numbered custom-sky sheet (cloud1.png,
// cloud2.png, ...) gets a chance to pick which one is the pack's actual
// sky before porting; any others can be ported separately as their own
// sky-only packs instead of just being ignored.
let skyPrimaryID = null;
let skyExtraIDs = new Set();

async function detectSkyOptions(file) {
  const body = new FormData();
  body.append("pack", file, file.name);
  const response = await fetch("/api/skies", { method: "POST", body });
  if (!response.ok) return [];
  const data = await response.json();
  return Array.isArray(data.skies) ? data.skies : [];
}

function renderSkyOptions(skies) {
  skyOptionsContainer.innerHTML = "";
  for (const sky of skies) {
    const card = document.createElement("div");
    card.className = "sky-option";
    card.dataset.id = sky.id;

    const imageWrap = document.createElement("div");
    imageWrap.className = "sky-option-image-wrap";
    const image = document.createElement("img");
    image.className = "sky-option-image";
    image.src = sky.thumbnailPng;
    image.alt = sky.label;
    image.addEventListener("click", () => selectPrimarySky(sky.id));
    imageWrap.append(image);
    if (sky.id === skyPrimaryID) {
      const badge = document.createElement("span");
      badge.className = "sky-option-badge";
      badge.textContent = "Pack sky";
      imageWrap.append(badge);
    }

    const label = document.createElement("p");
    label.className = "sky-option-label";
    label.textContent = sky.label;

    const extraButton = document.createElement("button");
    extraButton.type = "button";
    extraButton.className = "sky-option-extra";
    extraButton.textContent = skyExtraIDs.has(sky.id) ? "− Remove extra" : "+ Extra pack";
    extraButton.addEventListener("click", () => toggleSkyExtra(sky.id, extraButton));

    card.append(imageWrap, label, extraButton);
    skyOptionsContainer.append(card);
  }
  updatePrimarySkyHighlight();
}

function selectPrimarySky(id) {
  skyPrimaryID = id;
  skyExtraIDs.delete(id);
  const skies = currentSkyOptions;
  renderSkyOptions(skies);
}

function toggleSkyExtra(id, button) {
  if (id === skyPrimaryID) return;
  if (skyExtraIDs.has(id)) {
    skyExtraIDs.delete(id);
    button.textContent = "+ Extra pack";
    button.classList.remove("is-selected");
  } else {
    skyExtraIDs.add(id);
    button.textContent = "− Remove extra";
    button.classList.add("is-selected");
  }
}

function updatePrimarySkyHighlight() {
  for (const card of skyOptionsContainer.querySelectorAll(".sky-option")) {
    card.classList.toggle("is-primary", card.dataset.id === skyPrimaryID);
  }
}

let currentSkyOptions = [];

function openSkyModal(skies) {
  currentSkyOptions = skies;
  skyPrimaryID = skies[0].id;
  skyExtraIDs = new Set();
  renderSkyOptions(skies);
  skyModal.hidden = false;
  document.body.classList.add("modal-open");
  playSound("tap");
}

function closeSkyModal() {
  skyModal.hidden = true;
  document.body.classList.remove("modal-open");
}

skyModalBackdrop.addEventListener("click", closeSkyModal);
skyCancelButton.addEventListener("click", closeSkyModal);

skyAllExtrasButton.addEventListener("click", () => {
  for (const sky of currentSkyOptions) {
    if (sky.id !== skyPrimaryID) skyExtraIDs.add(sky.id);
  }
  renderSkyOptions(currentSkyOptions);
});

skySkipButton.addEventListener("click", () => {
  closeSkyModal();
  beginPort(null);
});

skyPortButton.addEventListener("click", () => {
  const extras = Array.from(skyExtraIDs).filter((id) => id !== skyPrimaryID);
  closeSkyModal();
  beginPort({ primary: skyPrimaryID, extras });
});

// ------------------------------------------------------------------ port
function lockPortingControls() {
  convertButton.disabled = true;
  removeFile.disabled = true;
  apiKeyInput.disabled = true;
  userIDInput.disabled = true;
  secretToggle.disabled = true;
  convertButton.textContent = "Porting...";
}

function unlockPortingControls() {
  removeFile.disabled = false;
  apiKeyInput.disabled = false;
  userIDInput.disabled = false;
  secretToggle.disabled = false;
  convertButton.textContent = "Port pack";
  updateConvertState();
}

async function beginPort(skyChoice) {
  resetOutput();
  lockPortingControls();
  playSound("start");
  updateProgress(1, "Porting...", `Sending ${currentFile.name}`);
  appendLog(`Sending ${currentFile.name}`);
  const body = new FormData();
  if (skyChoice && skyChoice.primary) body.append("skyPrimary", skyChoice.primary);
  if (skyChoice && skyChoice.extras && skyChoice.extras.length) {
    body.append("skyExtras", JSON.stringify(skyChoice.extras));
  }
  body.append("pack", currentFile, currentFile.name);
  try {
    const response = await fetch("/api/convert", {
      method: "POST",
      headers: {
        "X-Cone-Roblox-Api-Key": apiKeyInput.value.trim(),
        "X-Cone-Roblox-User-Id": userIDInput.value.trim(),
      },
      body,
    });
    await consumeEvents(response);
  } catch (error) {
    showError(error instanceof Error ? error.message : String(error));
  } finally {
    unlockPortingControls();
  }
}

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!currentFile) return;
  setupGuide.open = false;
  // No sky picker anymore, just port it. The pipeline already defaults to
  // cloud1.png (the day sky) over night or anything else when nothing more
  // specific is requested.
  await beginPort(null);
});
