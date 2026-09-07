const cols = 6;

function size(bytes) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (bytes >= 1024 && i < units.length - 1) { bytes /= 1024; i++; }
  return bytes.toFixed(i ? 1 : 0) + " " + units[i];
}

function age(iso) {
  let s = (Date.now() - new Date(iso)) / 1000;
  if (s < 60) return Math.floor(s) + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

function setText(node, value) {
  if (node.textContent !== String(value)) node.textContent = value;
}

function fileTree(paths, id) {
  const root = new Map();
  for (const fullPath of paths) {
    const path = fullPath.startsWith(id + "/") ? fullPath.slice(id.length + 1) : fullPath;
    let node = root;
    for (const part of path.split("/").filter(Boolean)) {
      if (!node.has(part)) node.set(part, new Map());
      node = node.get(part);
    }
  }
  return root;
}

// Directories first, then names naturally ordered, so part2 follows part1.
const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

function sortedEntries(tree) {
  return [...tree.entries()].sort(([aName, a], [bName, b]) =>
    (b.size > 0) - (a.size > 0) || collator.compare(aName, bName));
}

function reconcileTree(list, tree, prefix = "") {
  const existing = new Map([...list.children].map((item) => [item.dataset.key, item]));
  let position = list.firstElementChild;
  for (const [name, child] of sortedEntries(tree)) {
    const key = prefix ? prefix + "/" + name : name;
    const branch = child.size > 0;
    let item = existing.get(key);
    if (!item || (item.dataset.branch === "true") !== branch) {
      item?.remove();
      item = document.createElement("li");
      item.dataset.key = key;
      item.dataset.branch = branch;
      if (branch) {
        const details = document.createElement("details");
        details.open = true;
        const summary = document.createElement("summary");
        summary.className = "dir";
        const children = document.createElement("ul");
        details.append(summary, children);
        item.append(details);
      } else {
        const span = document.createElement("span");
        span.className = "file";
        item.append(span);
      }
    }
    existing.delete(key);
    if (branch) {
      setText(item.querySelector("summary"), name);
      reconcileTree(item.querySelector("ul"), child, key);
    } else {
      setText(item.firstElementChild, name);
    }
    if (item !== position) list.insertBefore(item, position);
    position = item.nextElementSibling;
  }
  for (const item of existing.values()) item.remove();
}

function updateFiles(cell, paths, id) {
  let details = cell.querySelector(".file-list");
  if (!paths.length) {
    details?.remove();
    return;
  }
  if (!details) {
    details = document.createElement("details");
    details.className = "file-list";
    const summary = document.createElement("summary");
    const tree = document.createElement("ul");
    tree.className = "file-tree";
    details.append(summary, tree);
    cell.append(details);
  }
  setText(details.querySelector(":scope > summary"), paths.length + (paths.length === 1 ? " file" : " files"));
  reconcileTree(details.querySelector(":scope > ul"), fileTree(paths, id));
}

function createRow(id, action) {
  const row = document.createElement("tr");
  row.dataset.id = id;
  for (let i = 0; i < cols; i++) row.insertCell();
  row.cells[0].className = "name";
  const title = document.createElement("div");
  title.className = "title";
  row.cells[0].append(title);
  const stage = document.createElement("span");
  row.cells[2].append(stage);
  const button = document.createElement("button");
  button.textContent = action === "cancel" ? "Cancel" : "Delete";
  button.onclick = () => remove(id, action, button);
  row.cells[5].append(button);
  return row;
}

function render(tbody, items, action, files = {}) {
  const existing = new Map([...tbody.querySelectorAll(":scope > tr[data-id]")].map((row) => [row.dataset.id, row]));
  const sort = sorts[tbody.id];
  markSorted(tbody, sort);
  tbody.querySelector(":scope > tr.empty")?.remove();
  if (!items.length) {
    const tr = tbody.insertRow();
    tr.className = "empty";
    const td = tr.insertCell();
    td.colSpan = cols;
    td.textContent = "nothing here";
    for (const row of existing.values()) row.remove();
    return;
  }
  let position = tbody.firstElementChild;
  for (const item of sortItems(items, sort)) {
    const tr = existing.get(item.id) || createRow(item.id, action);
    existing.delete(item.id);
    const name = tr.cells[0];
    setText(name.querySelector(".title"), item.id);
    let err = name.querySelector(".err");
    if (item.error) {
      if (!err) {
        err = document.createElement("div");
        err.className = "err";
        name.querySelector(".title").after(err);
      }
      setText(err, item.error);
    } else {
      err?.remove();
    }
    if (action === "delete") updateFiles(name, files[item.id] || [], item.id);
    setText(tr.cells[1], item.category || "");
    const stage = tr.cells[2].firstElementChild;
    stage.className = "stage " + item.stage;
    setText(stage, item.stage);
    setText(tr.cells[3], size(item.bytes));
    setText(tr.cells[4], age(item.added));
    if (tr !== position) tbody.insertBefore(tr, position);
    position = tr.nextElementSibling;
  }
  for (const row of existing.values()) row.remove();
}

// Age ascending is added descending, so the sort value is a negated timestamp
// and every column compares the same way.
function sortValue(item, key) {
  switch (key) {
    case "name": return item.id;
    case "category": return item.category || "";
    case "stage": return item.stage;
    case "bytes": return item.bytes;
    default: return -Date.parse(item.added);
  }
}

const sorts = { queue: { key: "age", dir: 1 }, history: { key: "age", dir: 1 } };

function sortItems(items, sort) {
  return items.slice().sort((a, b) => {
    const x = sortValue(a, sort.key), y = sortValue(b, sort.key);
    const order = typeof x === "string" ? collator.compare(x, y) : x - y;
    return sort.dir * (order || collator.compare(a.id, b.id));
  });
}

function markSorted(tbody, sort) {
  for (const th of tbody.closest("table").querySelectorAll("th[data-key]")) {
    th.dataset.sorted = th.dataset.key === sort.key ? (sort.dir > 0 ? "asc" : "desc") : "";
  }
}

for (const th of document.querySelectorAll("th[data-key]")) {
  th.onclick = () => {
    const tbody = th.closest("table").querySelector("tbody");
    const sort = sorts[tbody.id];
    sort.dir = sort.key === th.dataset.key ? -sort.dir : 1;
    sort.key = th.dataset.key;
    poll();
  };
}

async function remove(id, action, button) {
  button.disabled = true;
  try {
    const body = new URLSearchParams({ id, action });
    const response = await fetch("/api/remove", { method: "POST", body });
    if (!response.ok) {
      alert((await response.json()).error);
      button.disabled = false;
      return;
    }
    poll();
  } catch {
    button.disabled = false;
    document.getElementById("offline").classList.add("on");
  }
}

let polling = false;

async function poll() {
  if (polling) return;
  polling = true;
  try {
    const response = await fetch("/api/items");
    if (!response.ok) throw new Error(response.status);
    const data = await response.json();
    render(document.getElementById("queue"), data.queue, "cancel");
    render(document.getElementById("history"), data.history, "delete", data.files || {});
    document.getElementById("offline").classList.remove("on");
  } catch {
    // leave the last render up; an empty table would read as "nothing added"
    document.getElementById("offline").classList.add("on");
  } finally {
    polling = false;
  }
}

document.getElementById("add").onsubmit = async (event) => {
  event.preventDefault();
  const form = event.target;
  for (const file of form.file.files) {
    const body = new FormData();
    body.append("file", file);
    body.append("category", form.category.value);
    const response = await fetch("/api/add", { method: "POST", body });
    if (!response.ok) alert(file.name + ": " + (await response.json()).error);
  }
  form.reset();
  poll();
};

setInterval(() => { if (!document.hidden) poll(); }, 2000);
document.addEventListener("visibilitychange", () => { if (!document.hidden) poll(); });
poll();
