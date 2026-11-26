"use strict";

const byId = id => document.getElementById(id);
const tabs = [...document.querySelectorAll("[data-tab]")];
const timeFormat = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });
const numberFormat = new Intl.NumberFormat();
let key = "";
let identity = null;
let activeTab = "messages";
let agents = [];
let channels = new Set();
let memoryLoaded = false;
let currentRequest = null;
let timer;

function element(tag, text = "", className = "") {
  const node = document.createElement(tag);
  node.textContent = text;
  if (className) node.className = className;
  return node;
}

function humanTime(value) {
  const time = new Date(value);
  return value && Number.isFinite(time.getTime()) ? timeFormat.format(time) : "—";
}

function formatted(value) {
  return Number.isFinite(value) ? numberFormat.format(value) : "—";
}

function empty(id, message) {
  byId(id).replaceChildren(element("div", message, "empty"));
}

function setStatus(text, state) {
  byId("status").textContent = text;
  byId("status").dataset.state = state;
}

function recent(agent) {
  return agent.status === "online" && Date.now() - Date.parse(agent.last_seen) <= 90_000;
}

async function request(path, signal, credential = key, acceptUnavailable = false) {
  const options = { cache: "no-store", redirect: "error", signal, headers: {} };
  if (credential) options.headers["X-Murmur-Key"] = credential;
  const response = await fetch(path, options);
  if (response.ok || (acceptUnavailable && response.status === 503)) return response.json();
  const error = new Error(`HTTP ${response.status}`);
  error.status = response.status;
  throw error;
}

function showAccess(show, focus = false) {
  byId("access-panel").hidden = !show;
  byId("access").setAttribute("aria-expanded", String(show));
  if (show && focus) byId("key").focus();
}

function requestLabel(error, fallback) {
  if (error.status === 401) return "Connect with a Murmur API key to view this data.";
  if (error.status === 403) return "This key cannot view this data.";
  return fallback;
}

function showIdentity() {
  byId("access").textContent = identity ? (identity.admin ? "Operator access" : identity.agent) : "Use API key";
  byId("forget").hidden = !identity;
  byId("access-note").textContent = identity
    ? (identity.admin ? "Viewing the bus as an administrator. The key is held in this page." : `Viewing as ${identity.agent}. The key is held in this page.`)
    : "Use an agent key for its view, or the admin key for the full bus and enrollment status.";
}

function setIdentity(credential, actor) {
  currentRequest?.abort();
  key = credential;
  identity = actor;
  agents = [];
  channels = new Set();
  byId("channel").replaceChildren(new Option("All channels", ""));
  byId("memory-scope").replaceChildren(new Option("All scopes", ""), new Option("Shared", "shared"));
  byId("agent-count").textContent = "—";
  byId("memory-count").textContent = "—";
  for (const id of ["messages", "agents", "memories"]) byId(id).replaceChildren();
  showIdentity();
  load(true);
}

function chooseTab(name, focus = false) {
  activeTab = name;
  for (const tab of tabs) {
    const selected = tab.dataset.tab === name;
    tab.setAttribute("aria-selected", String(selected));
    tab.tabIndex = selected ? 0 : -1;
    byId(tab.getAttribute("aria-controls")).hidden = !selected;
    if (selected && focus) tab.focus();
  }
  load(activeTab === "memory");
}

function updateOptions(select, initial, values) {
  const selected = select.value;
  select.replaceChildren(new Option(initial, ""));
  for (const [value, label] of values) select.add(new Option(label, value));
  if (selected && ![...select.options].some(option => option.value === selected)) {
    select.add(new Option(selected, selected));
  }
  select.value = selected;
}

function renderMessages(items) {
  byId("message-summary").textContent = items.length ? `Showing ${items.length} newest in this view` : "No messages in this view";
  if (!items.length) {
    empty("messages", "No messages to show.");
    return;
  }
  for (const message of items) if (message.channel) channels.add(message.channel);
  updateOptions(byId("channel"), "All channels", [...channels].sort().map(channel => [channel, `#${channel}`]));
  const nodes = items.map(message => {
    const item = element("article", "", "record");
    const head = element("div", "", "record-header");
    head.append(element("strong", message.sender + (message.to ? ` → ${message.to}` : "")));
    if (message.channel) head.append(element("span", `#${message.channel}`, "badge accent"));
    if (message.status) head.append(element("span", message.status, "badge"));
    head.append(element("time", humanTime(message.created_at), "record-time"));
    item.append(head, element("p", message.message, "record-text"));
    const metadata = [];
    if (message.reply_to) metadata.push(`Reply to #${message.reply_to}`);
    if (message.session_id) metadata.push(`Session ${message.session_id.slice(0, 8)}`);
    if (message.metadata && Object.keys(message.metadata).length) metadata.push(JSON.stringify(message.metadata));
    if (metadata.length) item.append(element("p", metadata.join(" · "), "record-meta"));
    return item;
  });
  byId("messages").replaceChildren(...nodes);
}

function renderAgents() {
  const query = byId("agent-query").value.trim().toLowerCase();
  const matched = agents.filter(agent => [agent.name, agent.role, ...(agent.groups || [])].some(value => value.toLowerCase().includes(query)));
  byId("agent-summary").textContent = query ? `${matched.length} matching agents` : `${agents.length} registered · activity based on the last 90 seconds`;
  if (!matched.length) {
    empty("agents", query ? "No agents match this filter." : "No agents have registered.");
    return;
  }

  const table = element("table");
  const headers = table.createTHead().insertRow();
  const labels = ["Agent", "Activity", "Role and groups"];
  if (identity?.admin) labels.push("Credential");
  labels.push("Last seen");
  for (const label of labels) {
    const heading = element("th", label);
    heading.scope = "col";
    headers.append(heading);
  }

  const body = table.createTBody();
  for (const agent of matched) {
    const row = body.insertRow();
    const who = row.insertCell();
    who.append(element("strong", agent.name));
    if (agent.session_id) who.append(element("small", `Session ${agent.session_id.slice(0, 8)}`));
    if (agent.description) who.append(element("small", agent.description));
    const online = recent(agent);
    row.insertCell().append(element("span", online ? "Active" : "Idle", online ? "presence recent" : "presence"));
    const role = row.insertCell();
    role.append(element("span", agent.role || "—"));
    if (agent.memory_lead) role.append(" ", element("span", "Memory lead", "badge accent"));
    if (agent.groups?.length) role.append(element("small", agent.groups.map(group => `@${group}`).join(", ")));
    if (identity?.admin) row.insertCell().append(element("span", agent.key_enrolled ? "Enrolled" : "No key", agent.key_enrolled ? "badge positive" : "badge"));
    row.insertCell().textContent = humanTime(agent.last_seen);
  }
  const container = element("div", "", "table-wrap");
  container.append(table);
  byId("agents").replaceChildren(container);
}

function renderMemories(data) {
  memoryLoaded = true;
  byId("memory-summary").textContent = `${formatted(data.total)} matching current entries` + (data.total > data.items.length ? ` · showing ${data.items.length}` : "");
  if (!data.items.length) {
    empty("memories", "No memory entries match these filters.");
    return;
  }
  const nodes = data.items.map(entry => {
    const item = element("article", "", "record");
    const head = element("div", "", "record-header");
    head.append(element("strong", entry.key || entry.kind));
    const promoted = entry.provenance === "promoted";
    head.append(element("span", promoted ? "Project fact" : "Reported note", promoted ? "badge positive" : "badge"));
    head.append(element("span", entry.scope, "badge accent"), element("time", humanTime(entry.created_at), "record-time"));
    item.append(head, element("p", entry.body, "record-text"));
    const details = [`By ${entry.author}`];
    if (entry.promoted_by) details.push(`Promoted by ${entry.promoted_by}`);
    if (entry.origin_scope) details.push(`From ${entry.origin_scope}`);
    if (entry.source === "capture") details.push("Automatic capture");
    if (entry.expires_at) details.push(`Expires ${humanTime(entry.expires_at)}`);
    if (entry.tags?.length) details.push(entry.tags.join(", "));
    item.append(element("p", details.join(" · "), "record-meta"));
    return item;
  });
  byId("memories").replaceChildren(...nodes);
}

async function refresh(signal, searchMemory) {
  const messageQuery = new URLSearchParams({channel: byId("channel").value, limit: "40", order: "desc"});
  const messages = activeTab === "messages"
    ? request(`/messages?${messageQuery}`, signal) : Promise.resolve(null);
  const [healthResult, agentResult, messageResult] = await Promise.allSettled([
    request("/health", signal, "", true),
    request("/agents", signal),
    messages,
  ]);
  if (signal.aborted) return;

  const health = healthResult.status === "fulfilled" ? healthResult.value : null;
  setStatus(health ? (health.status === "ok" ? "Bus online" : "Bus degraded") : "Bus unavailable", health ? health.status : "error");
  byId("message-count").textContent = formatted(health?.messages);
  byId("message-detail").textContent = health?.uptime ? `Uptime ${health.uptime}` : "Message history";
  byId("memory-detail").textContent = health?.memory === "ok" ? "Store online" : (health?.memory ? "Store unavailable" : "Requires authenticated mode");

  if (agentResult.status === "fulfilled") {
    agents = agentResult.value;
    byId("agent-count").textContent = formatted(agents.filter(recent).length);
    byId("agent-detail").textContent = `${formatted(agents.length)} registered`;
    renderAgents();
  } else {
    agents = [];
    byId("agent-count").textContent = "—";
    byId("agent-detail").textContent = health ? `${formatted(health.agents)} registered` : "Roster unavailable";
    byId("agent-summary").textContent = "Roster unavailable";
    empty("agents", requestLabel(agentResult.reason, "Unable to load agents."));
    if (agentResult.reason.status === 401 && !key) showAccess(true);
  }

  if (messageResult.value) {
    renderMessages(messageResult.value.messages);
  } else if (messageResult.status === "rejected") {
    byId("message-summary").textContent = "History unavailable";
    empty("messages", requestLabel(messageResult.reason, "Unable to load message history."));
  }

  if (health?.memory !== "ok") {
    byId("memory-count").textContent = "—";
    byId("memory-summary").textContent = byId("memory-detail").textContent;
    memoryLoaded = false;
    empty("memories", health?.memory ? "Unable to reach the memory store." : "Memory becomes available when Murmur uses required authentication.");
    return;
  }

  const shouldLoadMemory = activeTab === "memory" && (searchMemory || !memoryLoaded);
  const filters = new URLSearchParams({limit: "30"});
  for (const [name, id] of [["q", "memory-query"], ["scope", "memory-scope"], ["kind", "memory-kind"]]) {
    if (byId(id).value) filters.set(name, byId(id).value);
  }
  const [stats, results] = await Promise.allSettled([
    request("/memories/stats", signal),
    shouldLoadMemory ? request(`/memories?${filters}`, signal) : Promise.resolve(null),
  ]);
  if (signal.aborted) return;
  if (stats.status === "fulfilled") {
    const scopes = Object.entries(stats.value.counts_per_scope || {}).sort(([a], [b]) => a.localeCompare(b));
    byId("memory-count").textContent = formatted(scopes.reduce((total, [, count]) => total + count, 0));
    byId("memory-detail").textContent = `${scopes.length} ${scopes.length === 1 ? "scope" : "scopes"}`;
    const scopeOptions = new Map([["shared", "Shared"]]);
    for (const [scope, count] of scopes) scopeOptions.set(scope, `${scope} (${count})`);
    updateOptions(byId("memory-scope"), "All scopes", scopeOptions);
  } else {
    byId("memory-count").textContent = "—";
    if (stats.reason.status === 401) byId("memory-detail").textContent = "API key required";
  }
  if (results.value) {
    renderMemories(results.value);
  } else if (results.status === "rejected") {
    byId("memory-summary").textContent = "Memory search unavailable";
    memoryLoaded = results.reason.status === 401 || results.reason.status === 403;
    empty("memories", requestLabel(results.reason, "Unable to search memory."));
  }
}

function load(searchMemory = false) {
  clearTimeout(timer);
  currentRequest?.abort();
  const controller = new AbortController();
  currentRequest = controller;
  refresh(controller.signal, searchMemory).catch(error => {
    if (!controller.signal.aborted) setStatus("Dashboard unavailable", "error");
    console.error("Murmur dashboard refresh failed", error);
  }).finally(() => {
    if (!controller.signal.aborted) timer = setTimeout(load, 5_000);
  });
}

for (const [index, tab] of tabs.entries()) {
  tab.addEventListener("click", () => chooseTab(tab.dataset.tab));
  tab.addEventListener("keydown", event => {
    const indices = { ArrowLeft: (index + tabs.length - 1) % tabs.length, ArrowRight: (index + 1) % tabs.length, Home: 0, End: tabs.length - 1 };
    if (Object.hasOwn(indices, event.key)) {
      event.preventDefault();
      chooseTab(tabs[indices[event.key]].dataset.tab, true);
    }
  });
}
byId("access").addEventListener("click", () => showAccess(byId("access-panel").hidden, true));
byId("refresh").addEventListener("click", () => load(true));
byId("channel").addEventListener("change", () => load());
byId("agent-query").addEventListener("input", renderAgents);
byId("memory-form").addEventListener("submit", event => {
  event.preventDefault();
  load(true);
});
byId("forget").addEventListener("click", () => {
  byId("key").value = "";
  byId("access-error").hidden = true;
  setIdentity("", null);
});
byId("key-form").addEventListener("submit", async event => {
  event.preventDefault();
  const submit = event.currentTarget.querySelector("button[type=submit]");
  const candidate = byId("key").value.trim();
  if (!candidate) return;
  submit.disabled = true;
  byId("access-error").hidden = true;
  try {
    const actor = await request("/identity", AbortSignal.timeout(10_000), candidate);
    setIdentity(candidate, actor);
    byId("key").value = "";
    showAccess(false);
    byId("access").focus();
  } catch (error) {
    byId("access-error").textContent = error.status === 401 ? "Murmur rejected this key." : "Unable to verify the key. Try again.";
    byId("access-error").hidden = false;
  } finally {
    submit.disabled = false;
  }
});
load();
