const cols = 8;

function size(bytes) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (bytes >= 1024 && i < units.length - 1) { bytes /= 1024; i++; }
  return bytes.toFixed(i ? 1 : 0) + " " + units[i];
}

function age(iso) {
  // A clock a second ahead of ours would otherwise read as a negative age
  let s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if (s < 60) return Math.floor(s) + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

function setText(node, value) {
  if (node.textContent !== String(value)) node.textContent = value;
}

// A release name is separator soup, so a line may break after any run of them
// rather than only at a space or a dash. A token still too long for the column
// falls back to the css, which breaks it anywhere.
function setBreakable(node, value) {
  const text = String(value);
  if (node.textContent === text) return;
  const parts = text.split(/(?<=[^\p{L}\p{N}])(?=[\p{L}\p{N}])/u);
  node.replaceChildren(...parts.flatMap((part) => [part, document.createElement("wbr")]).slice(0, -1));
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
      setBreakable(item.querySelector("summary"), name);
      reconcileTree(item.querySelector("ul"), child, key);
    } else {
      setBreakable(item.firstElementChild, name);
    }
    if (item !== position) list.insertBefore(item, position);
    position = item.nextElementSibling;
  }
  for (const item of existing.values()) item.remove();
}

// The tree gets the full table width as a row of its own, and the toggle stays
// under the name where it belongs - which a <details> spanning both cannot do.
function updateFiles(row, paths, id) {
  const name = row.cells[0];
  let toggle = name.querySelector(".tree-toggle");
  if (!paths.length) {
    toggle?.remove();
    row.filesRow?.remove();
    row.filesRow = null;
    return null;
  }
  let filesRow = row.filesRow;
  if (!filesRow) {
    filesRow = document.createElement("tr");
    filesRow.className = "file-list";
    filesRow.hidden = true;
    const cell = filesRow.insertCell();
    cell.colSpan = cols;
    const tree = document.createElement("ul");
    tree.className = "file-tree";
    cell.append(tree);
    row.filesRow = filesRow;
  }
  if (!toggle) {
    toggle = document.createElement("button");
    toggle.className = "files-toggle tree-toggle";
    toggle.onclick = () => {
      filesRow.hidden = !filesRow.hidden;
      toggle.dataset.open = !filesRow.hidden;
    };
    toggle.dataset.open = !filesRow.hidden;
    name.querySelector(".toggles").append(toggle);
  }
  row.after(filesRow);
  setText(toggle, paths.length + (paths.length === 1 ? " file" : " files"));
  reconcileTree(filesRow.querySelector("ul"), fileTree(paths, id));
  return filesRow;
}

// The strip is what the whole process is doing; a row says what one nzb of it
// holds. One bubble per thing being reported, since the numbers inside it only
// mean something together.
function strip(stats) {
  const target = document.getElementById("stats");
  if (!stats.cache) {
    target.hidden = true;
    return;
  }
  target.hidden = false;

  const cache = stats.cache;
  const reads = cache.hits + cache.misses;
  const groups = [
    ["cache", [
      ["used", cache.max_bytes ? `${size(cache.bytes)} / ${size(cache.max_bytes)}` : size(cache.bytes)],
      ["segments", cache.items],
      ["hit rate", reads ? percent(cache.hits, reads) : "-"],
      ["evictions", cache.evictions],
    ]],
    ["usenet", [["servers up", `${stats.servers.up} / ${stats.servers.total}`]]],
  ];

  target.replaceChildren(...groups.map(([title, values]) => {
    const bubble = document.createElement("div");
    bubble.className = "bubble";
    const name = document.createElement("b");
    name.textContent = title;
    bubble.append(name);
    for (const [label, value] of values) {
      const entry = document.createElement("span");
      entry.append(label);
      const number = document.createElement("strong");
      number.textContent = value;
      entry.append(number);
      bubble.append(entry);
    }
    return bubble;
  }));
}

// The stage values are the api's; the column only has room for the short form
// of the long ones.
const stageLabels = { completed: "done", cancelled: "stop", rebuilding: "rebuild" };

// A size covering a segment nothing has decoded yet is a lower bound on it
function estimated(bytes, exact) {
  return (exact ? "" : "~") + size(bytes);
}

function percent(part, whole) {
  return whole ? Math.round(100 * part / whole) + "%" : "0%";
}

function cell(row, text, tag = "td") {
  const cell = document.createElement(tag);
  cell.textContent = text;
  row.append(cell);
}

function renderInfo(target, data) {
  const total = document.createElement("div");
  total.className = "info-total";
  total.textContent = `${size(data.cached_bytes)} of ${estimated(data.bytes, data.exact)} cached`
    + ` (${percent(data.cached_bytes, data.bytes)}),`
    + ` ${data.cached_segments} of ${data.segments} segments`;

  // A value and what it is out of are a column each, so they line up down the
  // table rather than each row setting its own width.
  const table = document.createElement("table");
  table.className = "info-files";
  const head = table.createTHead().insertRow();
  for (const [label, span] of [["Posted file", 1], ["Size", 1], ["Cached", 2], ["Segments", 2], ["Read", 1]]) {
    cell(head, label, "th");
    head.lastElementChild.colSpan = span;
  }
  // The order an nzb lists its files in is the posters, so vol03 lands before
  // vol01. The tree reads in name order and so does this.
  const body = table.createTBody();
  for (const file of data.files.slice().sort((a, b) => collator.compare(a.name, b.name))) {
    const row = body.insertRow();
    setBreakable(row.insertCell(), file.name);
    cell(row, estimated(file.bytes, file.exact));
    cell(row, size(file.cached_bytes));
    cell(row, `(${percent(file.cached_bytes, file.bytes)})`);
    cell(row, file.cached_segments + " /");
    cell(row, file.segments);
    cell(row, file.cached_segments ? age(file.last_read) : "-");
  }

  target.replaceChildren(total, table);
}

// The detail is per segment of every posted file, which is why it is fetched
// for an open panel only rather than carried by the poll.
async function loadInfo(id, infoRow) {
  try {
    const response = await fetch("/api/nzb?id=" + encodeURIComponent(id));
    if (!response.ok) throw new Error(response.status);
    renderInfo(infoRow.cells[0], await response.json());
  } catch {
    setText(infoRow.cells[0], "no detail for this one yet");
  }
}

// The panel costs a lookup per segment of one nzb, so it follows the poll while
// it is open and nothing at all while it is not.
function updateInfo(row, item) {
  let infoRow = row.infoRow;
  if (!infoRow) {
    infoRow = document.createElement("tr");
    infoRow.className = "info";
    infoRow.hidden = true;
    infoRow.insertCell().colSpan = cols;
    row.infoRow = infoRow;

    const toggle = document.createElement("button");
    toggle.className = "files-toggle stats-toggle";
    toggle.textContent = "stats";
    toggle.dataset.open = "false";
    toggle.onclick = () => {
      infoRow.hidden = !infoRow.hidden;
      toggle.dataset.open = !infoRow.hidden;
      if (!infoRow.hidden) loadInfo(row.dataset.id, infoRow);
    };
    row.cells[0].querySelector(".toggles").append(toggle);
  }
  (row.filesRow || row).after(infoRow);
  if (!infoRow.hidden) loadInfo(item.id, infoRow);
  return infoRow;
}

function removeRow(row) {
  row.infoRow?.remove();
  row.filesRow?.remove();
  row.remove();
}

function createRow(id, action) {
  const row = document.createElement("tr");
  row.dataset.id = id;
  for (let i = 0; i < cols; i++) row.insertCell();
  row.cells[0].className = "name";
  const title = document.createElement("div");
  title.className = "title";
  const toggles = document.createElement("div");
  toggles.className = "toggles";
  const inner = document.createElement("div");
  inner.className = "name-cell";
  inner.append(title, toggles);
  row.cells[0].append(inner);
  const stage = document.createElement("span");
  row.cells[2].append(stage);
  if (action === "delete") {
    const archive = document.createElement("button");
    archive.className = "archive";
    row.cells[7].append(archive);
  }
  const button = document.createElement("button");
  button.className = action === "cancel" ? "" : "danger";
  button.textContent = action === "cancel" ? "Cancel" : "Delete";
  button.onclick = () => remove(id, action, button);
  row.cells[7].append(button);
  return row;
}

function render(tbody, items, action, files = {}) {
  const existing = new Map([...tbody.querySelectorAll(":scope > tr[data-id]")].map((row) => [row.dataset.id, row]));
  const sort = sorts[tbody.id];
  markSorted(tbody, sort);
  const page = paginate(tbody.id, sortItems(items, sort));
  setText(document.querySelector(`.count[data-for="${tbody.id}"]`),
    items.length ? `(${items.length} ${items.length === 1 ? "item" : "items"})` : "");
  tbody.querySelector(":scope > tr.empty")?.remove();
  if (!page.length) {
    const tr = tbody.insertRow();
    tr.className = "empty";
    const td = tr.insertCell();
    td.colSpan = cols;
    td.textContent = "nothing here";
    for (const row of existing.values()) removeRow(row);
    return;
  }
  let position = tbody.firstElementChild;
  for (const item of page) {
    const tr = existing.get(item.id) || createRow(item.id, action);
    existing.delete(item.id);
    const name = tr.cells[0];
    setBreakable(name.querySelector(".title"), item.id);
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
    const archive = tr.cells[7].querySelector(".archive");
    if (archive) {
      const next = item.archived ? "restore" : "archive";
      setText(archive, item.archived ? "Restore" : "Archive");
      archive.onclick = () => remove(item.id, next, archive);
    }
    setText(tr.cells[1], item.category || "");
    const stage = tr.cells[2].firstElementChild;
    stage.className = "stage " + item.stage;
    setText(stage, stageLabels[item.stage] || item.stage);
    setText(tr.cells[3], estimated(item.bytes, item.bytes_exact));
    setText(tr.cells[4], item.cached ? size(item.cached) : "-");
    setText(tr.cells[5], age(item.added));
    setText(tr.cells[6], item.read ? age(item.read) : "-");
    if (tr !== position) tbody.insertBefore(tr, position);
    if (action === "delete") updateFiles(tr, files[item.id] || [], item.id);
    position = updateInfo(tr, item).nextElementSibling;
  }
  for (const row of existing.values()) removeRow(row);
}

// One poll carries every item, so a page is a slice of what is already here.
// The size is the one thing worth keeping between visits.
const perPage = document.getElementById("per-page");
perPage.value = localStorage.getItem("perPage") ?? "25";

const pages = { queue: 0, history: 0 };

function paginate(id, items) {
  const size = Number(perPage.value) || items.length;
  const last = Math.max(0, Math.ceil(items.length / size) - 1);
  // A page can fall off the end when items leave, and the sort or the filter
  // changing makes the one being looked at a different set anyway
  const page = pages[id] = Math.min(Math.max(pages[id], 0), last);
  const start = page * size;
  const shown = items.slice(start, start + size);

  const pager = document.querySelector(`.pager[data-for="${id}"]`);
  setText(pager.firstElementChild, `${start + 1}-${start + shown.length} of ${items.length}`);
  pager.querySelector("[data-step='-1']").disabled = page === 0;
  pager.querySelector("[data-step='1']").disabled = page === last;
  pager.hidden = last === 0;

  return shown;
}

perPage.onchange = () => {
  localStorage.setItem("perPage", perPage.value);
  pages.queue = pages.history = 0;
  poll();
};

for (const button of document.querySelectorAll(".pager button")) {
  button.onclick = () => {
    pages[button.closest(".pager").dataset.for] += Number(button.dataset.step);
    poll();
  };
}

// Age ascending is added descending, so the sort value is a negated timestamp
// and every column compares the same way.
function sortValue(item, key) {
  switch (key) {
    case "name": return item.id;
    case "category": return item.category || "";
    case "stage": return item.stage;
    case "bytes": return item.bytes;
    case "cached": return item.cached;
    case "read": return item.read ? -Date.parse(item.read) : Infinity;
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
    pages[tbody.id] = 0;
    poll();
  };
}

async function remove(id, action, button) {
  // Archiving is undone with Restore; deleting takes the files off the mount
  if (action === "delete" && !confirm("Delete " + id + "?\nThe files will stop being presented.")) return;
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

// An archived add is still presented; the flag only says which half of the
// history it belongs to.
const showArchived = document.querySelector("#show-archived input");
showArchived.onchange = () => {
  pages.history = 0;
  poll();
};

let polling = false;

async function poll() {
  if (polling) return;
  polling = true;
  try {
    const response = await fetch("/api/items");
    if (!response.ok) throw new Error(response.status);
    const data = await response.json();
    const cached = (data.stats || {}).cached || {};
    // Sorting on the column reads it off the item, like every other column
    for (const item of [...(data.queue || []), ...(data.history || [])]) {
      item.cached = (cached[item.id] || {}).bytes || 0;
      item.read = item.cached ? cached[item.id].last_read : "";
    }
    strip(data.stats || {});
    render(document.getElementById("queue"), data.queue, "cancel");
    const history = (data.history || []).filter((item) => !!item.archived === showArchived.checked);
    render(document.getElementById("history"), history, "delete", data.files || {});
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
