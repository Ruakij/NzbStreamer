const cols = 8;

function sizeParts(bytes) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (bytes >= 1024 && i < units.length - 1) { bytes /= 1024; i++; }
  return [bytes.toFixed(i ? 1 : 0), units[i]];
}

function size(bytes) {
  return sizeParts(bytes).join(" ");
}

function duration(s) {
  if (s < 60) return Math.floor(s) + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

function age(iso) {
  // A clock a second ahead of ours would otherwise read as a negative age
  return duration(Math.max(0, (Date.now() - new Date(iso)) / 1000));
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

// Icons are drawn inline rather than fetched, so they inherit the colour of the
// text they sit in and cost no request. One 24-grid path each.
const icons = {
  download: "M12 4v12m0 0 4-4m-4 4-4-4M5 20h14",
  nzb: "M14 3H6v18h12V7zm0 0v4h4M12 11v6m0 0 2.5-2.5M12 17l-2.5-2.5",
  add: "M12 5v14M5 12h14",
  inspect: "M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14m5.5 12.5L21 21",
  close: "M18 6 6 18M6 6l12 12",
  prev: "m15 6-6 6 6 6",
  next: "m9 6 6 6-6 6",
  offline: "M12 4 2.5 20.5h19zM12 10v4m0 3.5h.01",
  cancel: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18M7 7l10 10",
  delete: "M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13M10 11v6M14 11v6",
  archive: "M4 4h16v5H4zM6 9v12h12V9M10 13h4",
  restore: "M4 9h10a5 5 0 0 1 0 10H9M4 9l4-4M4 9l4 4",
  folder: "M3 6a1 1 0 0 1 1-1h5l2 2h9a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1z",
  folderOpen: "M3 19V6a1 1 0 0 1 1-1h5l2 2h7a1 1 0 0 1 1 1v2M3 19l3-8h16l-3 8z",
  file: "M14 3H6v18h12V7zm0 0v4h4",
  video: "M4 5h16v14H4zm6 3.5 5 3.5-5 3.5z",
  audio: "M9 17V5l10-2v12M9 17a2.5 2.5 0 1 1-5 0 2.5 2.5 0 0 1 5 0m10-2a2.5 2.5 0 1 1-5 0 2.5 2.5 0 0 1 5 0",
  image: "M4 5h16v14H4zm0 11 4-4 3 3 3-3 6 6M9.5 9.5a1 1 0 1 1-2 0 1 1 0 0 1 2 0",
  text: "M14 3H6v18h12V7zm0 0v4h4M9 12h6M9 16h4",
  stats: "M4 19h16M8 19v-6M13 19V6M18 19v-9",
  recovery: "M12 3 5 6v6c0 4.2 3 6.9 7 8 4-1.1 7-3.8 7-8V6zM9.5 12l2 2 3.5-4",
  show: "M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12m10-3a3 3 0 1 0 0 6 3 3 0 0 0 0-6",
};

function icon(name) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "2");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  svg.classList.add("icon");
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", icons[name]);
  svg.append(path);
  return svg;
}

// An action on a row is its icon
function setAction(node, name, text) {
  if (!node.firstElementChild) node.append(icon(name));
  node.firstElementChild.firstElementChild.setAttribute("d", icons[name]);
  node.dataset.act = name;
  node.dataset.hint = text;
  node.setAttribute("aria-label", text);
}

// One hint for the page
const hint = document.createElement("div");
hint.className = "hint";
hint.popover = "manual";
document.body.append(hint);

let hinting = null;
let hintTimer = 0;

function showHint(node) {
  if (node === hinting) return;
  hinting = node;
  clearTimeout(hintTimer);
  // Moving from one action to the next never leaves the page, so the open hint
  // is taken down here rather than by an event
  const open = hint.matches(":popover-open");
  if (open) hint.hidePopover();
  if (!node) return;
  // A control carrying no text cannot be read without its hint
  const wait = node.textContent.trim() && !open ? 1000 : 0;
  if (wait) hintTimer = setTimeout(() => drawHint(node), wait);
  else drawHint(node);
}

function drawHint(node) {
  setText(hint, node.dataset.hint);
  hint.showPopover();
  const box = node.getBoundingClientRect();
  const room = hint.getBoundingClientRect();
  // Under the anchor, unless the window ends first, and never past either edge
  hint.style.top = (box.bottom + room.height + 8 > innerHeight ? box.top - room.height - 6 : box.bottom + 6) + "px";
  hint.style.left = Math.min(Math.max(box.left + box.width / 2, room.width / 2 + 4), innerWidth - room.width / 2 - 4) + "px";
}

// Delegated, so an action rendered into a row later is hinted without wiring:
// moving onto anything that is not hinted takes the hint down with it
document.addEventListener("pointerover", (event) => showHint(event.target.closest("[data-hint]")));
document.addEventListener("pointerleave", () => showHint(null));
document.addEventListener("focusin", (event) => showHint(event.target.closest("[data-hint]")));
document.addEventListener("focusout", () => showHint(null));

// The chrome the page ships with is labelled here rather than in the html, so
// every icon on the page comes out of the one set above. The close is the only
// icon-only one: it was already a glyph rather than a word.
for (const [selector, name] of [["#offline", "offline"], ["#add button[type=submit]", "add"], ["#inspect", "inspect"]]) {
  document.querySelector(selector).prepend(icon(name));
}
document.querySelector("#inspect-head button").replaceChildren(icon("close"));

function fileTree(files, id) {
  const root = new Map();
  for (const file of files) {
    const path = file.path.startsWith(id + "/") ? file.path.slice(id.length + 1) : file.path;
    let node = root;
    for (const part of path.split("/").filter(Boolean)) {
      if (!node.has(part)) node.set(part, new Map());
      node = node.get(part);
    }
    node.file = file;
  }
  treeTotals(root);
  return root;
}

// A directory weighs what it holds, so a collapsed one still says how big it is
function treeTotals(tree) {
  let bytes = 0;
  let exact = true;
  for (const child of tree.values()) {
    const total = child.size > 0 ? treeTotals(child) : child.file;
    bytes += total.bytes;
    exact = exact && total.exact;
  }
  tree.total = { bytes, exact };
  return tree.total;
}

// Files are served over webdav, under the same origin as this page.
function webdavURL(path) {
  return "/webdav/" + path.split("/").map(encodeURIComponent).join("/");
}

// Directories first, then names naturally ordered, so part2 follows part1.
const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

// A posted file reads by its name, except that recovery carries none of the
// release and follows everything it can repair, rather than landing among the
// volumes because a digit sorts before a letter.
function byPostedName(a, b) {
  return (fileIcon(a) === "recovery") - (fileIcon(b) === "recovery") || collator.compare(a, b);
}

function sortedEntries(tree) {
  return [...tree.entries()].sort(([aName, a], [bName, b]) =>
    (b.size > 0) - (a.size > 0) || collator.compare(aName, bName));
}

// The posting date of the nzb, which is what every file of it is stamped with.
// A zero time is an nzb that posted no date and says nothing worth a column.
const dateFormat = new Intl.DateTimeFormat(undefined, { dateStyle: "short", timeStyle: "short" });

function fileDate(file) {
  const posted = new Date(file.date);
  return posted.getFullYear() > 1 ? dateFormat.format(posted) : "";
}

// The icon says what a file is before its name is read. The extensions are the
// ones a release carries; anything else is a page.
const iconExtensions = {
  video: ["mkv", "mp4", "avi", "m4v", "mov", "ts", "mpg", "mpeg", "wmv", "webm"],
  audio: ["mp3", "flac", "m4a", "aac", "ogg", "wav"],
  image: ["jpg", "jpeg", "png", "gif", "webp", "bmp"],
  archive: ["rar", "zip", "7z", "tar", "gz"],
  recovery: ["par2", "par"],
  text: ["nfo", "txt", "srt", "sub", "idx", "sfv", "log", "md"],
};
const iconByExtension = new Map(Object.entries(iconExtensions)
  .flatMap(([name, extensions]) => extensions.map((extension) => [extension, name])));

function fileIcon(name) {
  const extension = name.slice(name.lastIndexOf(".") + 1).toLowerCase();
  // A continuation volume of a split set: rar's r00, zip's z01, 7z's 001
  if (/^(r\d{2,3}|z\d{2}|\d{2,3})$/.test(extension)) return "archive";
  return iconByExtension.get(extension) || "file";
}

function span(className) {
  const node = document.createElement("span");
  node.className = className;
  return node;
}

// The icon of a row or a toggle, which says what it holds and whether it is open
function setKind(node, name) {
  node.querySelector(".kind").firstElementChild.setAttribute("d", icons[name]);
}

function kindIcon(name) {
  const glyph = icon(name);
  glyph.classList.add("kind");
  return glyph;
}

// Every row carries the same columns, and nesting only moves its left edge, so
// what is on the right lines up however deep the file sits.
function treeRow(tag, kind, label) {
  const row = document.createElement(tag);
  row.className = "row";
  row.dataset.kind = kind;
  row.append(kindIcon(kind), span(label), span("col size"), span("col date"), span("row-actions"));
  return row;
}

// A toggle names what its panel holds. It reads as pressed while the panel is
// open, which is what says whether it is.
function toggleButton(className, kind, hint) {
  const button = document.createElement("button");
  button.className = "files-toggle " + className;
  button.dataset.hint = hint;
  button.append(kindIcon(kind), span("label"));
  return button;
}

function setOpen(button, open) {
  button.dataset.open = open;
  button.setAttribute("aria-pressed", open);
}

function createBranch() {
  const item = document.createElement("li");
  const details = document.createElement("details");
  details.open = true;
  const summary = treeRow("summary", "folderOpen", "label dir");
  // The folder itself is the marker, so it is what opening the row changes
  details.ontoggle = () => setKind(summary, details.open ? "folderOpen" : "folder");
  summary.dataset.kind = "folder";
  const children = document.createElement("ul");
  details.append(summary, children);
  item.append(details);
  return item;
}

function createLeaf(name) {
  const item = document.createElement("li");
  const row = treeRow("div", fileIcon(name), "label file");
  const link = document.createElement("a");
  link.className = "act";
  link.dataset.act = "download";
  link.dataset.hint = "download this file";
  link.append(icon("download"));
  row.lastElementChild.append(link);
  item.append(row);
  return item;
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
      item = branch ? createBranch() : createLeaf(name);
      item.dataset.key = key;
      item.dataset.branch = branch;
    }
    existing.delete(key);
    const row = item.querySelector(".row");
    setBreakable(row.querySelector(".label"), name);
    if (branch) {
      setSize(row.querySelector(".size"), child.total.bytes, child.total.exact);
      reconcileTree(item.querySelector("ul"), child, key);
    } else {
      setSize(row.querySelector(".size"), child.file.bytes, child.file.exact);
      setText(row.querySelector(".date"), fileDate(child.file));
      const link = row.querySelector("a.act");
      link.href = webdavURL(child.file.path);
      link.download = name;
    }
    if (item !== position) list.insertBefore(item, position);
    position = item.nextElementSibling;
  }
  for (const item of existing.values()) item.remove();
}

// The tree gets the full table width as a row of its own, and the toggle stays
// under the name where it belongs - which a <details> spanning both cannot do.
function updateFiles(row, files, id) {
  const name = row.cells[0];
  let toggle = name.querySelector(".tree-toggle");
  if (!files.length) {
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
    toggle = toggleButton("tree-toggle", "folder", "files this Nzb presents");
    toggle.onclick = () => {
      filesRow.hidden = !filesRow.hidden;
      setOpen(toggle, !filesRow.hidden);
      setKind(toggle, filesRow.hidden ? "folder" : "folderOpen");
    };
    setOpen(toggle, !filesRow.hidden);
    name.querySelector(".toggles").append(toggle);
  }
  row.after(filesRow);
  setText(toggle.lastElementChild, files.length + (files.length === 1 ? " file" : " files"));
  reconcileTree(filesRow.querySelector("ul"), fileTree(files, id));
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
  const library = stats.library || {};
  const io = stats.io || {};
  const window_ = duration(library.window || 0);
  const groups = [
    // Nominal is everything added, active what of it was read within the window:
    // the cache has to hold the second, not the first
    ["library", [
      ["nominal", (library.exact ? "" : "~") + size(library.bytes) + (library.max_bytes ? ` / ${size(library.max_bytes)}` : ""),
        "decoded size of everything presented"],
      ["active", size(library.active), `bytes read in the last ${window_}`],
    ]],
    ["cache", [
      ["used", cache.max_bytes ? `${size(cache.bytes)} / ${size(cache.max_bytes)}` : size(cache.bytes),
        "cached bytes on disk"],
      ["hit rate", reads ? percent(cache.hits, reads) : "-", "reads served from cache"],
      // What the cache being smaller than the active library cost, which the
      // lifetime hit rate above cannot show once it has averaged out
      ["refetched", size(cache.refetched), `bytes downloaded more than once in the last ${window_}`],
    ]],
    ["usenet", [
      ["connections", `${stats.servers.conns} / ${stats.servers.max_conns}`, "open server connections"],
      ["in", rate("fetched", stats.servers.fetched), "download rate"],
      ["downloaded", size(stats.servers.fetched), "total downloaded"],
    ]],
    ["i/o", [
      ["open files", io.open, "files clients have open"],
      ["out", rate("served", io.served), "rate served to clients"],
      ["served", size(io.served), "total served to clients"],
    ]],
  ];

  target.replaceChildren(...groups.map(([title, values]) => {
    const bubble = document.createElement("div");
    bubble.className = "bubble";
    const name = document.createElement("b");
    name.textContent = title;
    bubble.append(name);
    for (const [label, value, hint] of values) {
      const entry = document.createElement("span");
      if (hint) entry.dataset.hint = hint;
      entry.append(label);
      const number = document.createElement("strong");
      if (String(value).endsWith("/s")) number.className = "rate";
      number.textContent = value;
      entry.append(number);
      bubble.append(entry);
    }
    return bubble;
  }));
}

// The stage values are the api's; the column only has room for the short form
// of the long ones.
const stageLabels = { completed: "done", cancelled: "stop", cancelling: "stopping", rebuilding: "rebuild" };

// An add still running carries how far it has got and what the api estimates is
// left of it, the wait for a slot included, so a queued one reads as a wait
// rather than as a stall. An eta of 0 is one nothing can estimate yet, which is
// what an add says while the servers have answered nothing to measure them by.
function renderProgress(cell, item) {
  const done = item.progress || 0;
  const running = !["completed", "failed", "cancelled", "cancelling"].includes(item.stage);
  const bar = cell.querySelector(".bar");
  bar.firstElementChild.style.width = `${Math.round(done * 100)}%`;
  bar.hidden = !running;
  const left = item.eta ? ` - ${duration(item.eta)}` : "";
  setText(cell.lastElementChild, running ? `${Math.round(done * 100)}%${left}` : "");
}

// A size covering a segment nothing has decoded yet is a lower bound on it
function estimated(bytes, exact) {
  return (exact ? "" : "~") + size(bytes);
}

// rate turns a counter into what it grew by per second, from the poll before
// this one. The server sends counters, so how fast they move is ours to work
// out; the first poll of a counter has nothing to compare against.
const counters = {};
function rate(name, total) {
  const now = Date.now();
  const last = counters[name];
  counters[name] = { total, at: now };
  if (!last || now === last.at) return "-";
  return `${size(Math.max(total - last.total, 0) * 1000 / (now - last.at))}/s`;
}

function percent(part, whole) {
  return whole ? Math.round(100 * part / whole) + "%" : "0%";
}

function cell(row, text, tag = "td") {
  const cell = document.createElement(tag);
  cell.textContent = text;
  row.append(cell);
}

// The number sits in a box of a fixed width, so what lines up down a column is
// the digits rather than the B of whichever unit each row happened to reach. The
// unit follows it directly, closer than any two columns are to each other.
function setSize(node, bytes, exact = true) {
  const [number, unit] = sizeParts(bytes);
  let suffix = node.querySelector(":scope > .unit");
  if (!suffix) {
    node.replaceChildren(span("num"), span("unit"));
    suffix = node.lastElementChild;
  }
  setText(node.firstElementChild, (exact ? "" : "~") + number);
  setText(suffix, unit);
}

function sizeCell(row, bytes, exact = true) {
  cell(row, "");
  setSize(row.lastElementChild, bytes, exact);
}

// A posted file is read the same way here as in the tree: what it is, then its
// name.
function nameCell(row, name) {
  const cell = row.insertCell();
  cell.dataset.kind = fileIcon(name);
  const label = span("label");
  setBreakable(label, name);
  cell.append(kindIcon(cell.dataset.kind), label);
  return cell;
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
  for (const [label, wide] of [["Posted file", 1], ["Size", 1], ["Cached", 2], ["Segments", 2], ["Read", 1]]) {
    cell(head, label, "th");
    head.lastElementChild.colSpan = wide;
  }
  // The order an nzb lists its files in is the posters, so vol03 lands before
  // vol01. The tree reads in name order and so does this.
  const body = table.createTBody();
  for (const file of data.files.slice().sort((a, b) => byPostedName(a.name, b.name))) {
    const row = body.insertRow();
    nameCell(row, file.name);
    sizeCell(row, file.bytes, file.exact);
    sizeCell(row, file.cached_bytes);
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
function updateInfo(row, item, presented) {
  let infoRow = row.infoRow;
  // The panel is what is cached of a presented tree, so a record that got as far
  // as presenting nothing has none: an add that failed or was cancelled
  if (!presented) {
    row.cells[0].querySelector(".stats-toggle")?.remove();
    infoRow?.remove();
    row.infoRow = null;
    return row;
  }
  if (!infoRow) {
    infoRow = document.createElement("tr");
    infoRow.className = "info";
    infoRow.hidden = true;
    infoRow.insertCell().colSpan = cols;
    row.infoRow = infoRow;

    const toggle = toggleButton("stats-toggle", "stats", "cached bytes per posted file");
    toggle.lastElementChild.textContent = "stats";
    setOpen(toggle, false);
    toggle.onclick = () => {
      infoRow.hidden = !infoRow.hidden;
      setOpen(toggle, !infoRow.hidden);
      if (!infoRow.hidden) loadInfo(row.dataset.id, infoRow);
    };
    row.cells[0].querySelector(".toggles").append(toggle);
  }
  (row.filesRow || row).after(infoRow);
  if (!infoRow.hidden) loadInfo(item.id, infoRow);
  return infoRow;
}

function removeRow(row) {
  // A row taken away under the pointer would otherwise leave its hint standing
  if (row.contains(hinting)) showHint(null);
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
  // What an add is doing and how far it has got read as one thing, so they
  // share a column: the stage, and under it what is left of it while it runs.
  row.cells[2].className = "stage-cell";
  const bar = document.createElement("div");
  bar.className = "bar";
  bar.append(document.createElement("div"));
  row.cells[2].append(document.createElement("span"), bar, document.createElement("small"));
  // The share is under the size and as wide as it needs, so it grows leftwards
  // under the number rather than pushing the column
  const stack = span("stack");
  stack.append(document.createElement("div"), span("share"));
  row.cells[4].append(stack);
  // Everything that acts on the nzb sits together in the last column
  const actions = document.createElement("div");
  actions.className = "actions";
  const nzb = document.createElement("a");
  nzb.className = "act";
  setAction(nzb, "nzb", "download the Nzb");
  nzb.href = "/api/nzb/file?id=" + encodeURIComponent(id);
  nzb.download = id + ".nzb";
  actions.append(nzb);
  if (action === "delete") {
    const archive = document.createElement("button");
    archive.className = "act archive";
    actions.append(archive);
  }
  const button = document.createElement("button");
  button.className = "act";
  setAction(button, action, action === "cancel"
    ? "cancel the add"
    : "delete, stops presenting the files");
  button.onclick = () => remove(id, action, button);
  actions.append(button);
  row.cells[7].append(actions);
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
      setAction(archive, next, item.archived
        ? "restore to the list"
        : "archive, hides it from the list");
      archive.onclick = () => remove(item.id, next, archive);
    }
    setText(tr.cells[1], item.category || "");
    const stage = tr.cells[2].firstElementChild;
    stage.className = "stage " + item.stage;
    setText(stage, stageLabels[item.stage] || item.stage);
    renderProgress(tr.cells[2], item);
    setSize(tr.cells[3], item.bytes, item.bytes_exact);
    const cached = tr.cells[4].firstElementChild;
    if (item.cached) setSize(cached.firstElementChild, item.cached);
    else setText(cached.firstElementChild, "-");
    setText(cached.lastElementChild,
      item.cached && item.bytes ? `(${percent(item.cached, item.bytes)})` : "");
    setText(tr.cells[5], age(item.added));
    setText(tr.cells[6], item.read ? age(item.read) : "-");
    if (tr !== position) tbody.insertBefore(tr, position);
    // A queue row presents as it builds, so it is only history that can be done
    // and have nothing
    const presented = files[item.id] || [];
    if (action === "delete") updateFiles(tr, presented, item.id);
    position = updateInfo(tr, item, action === "cancel" || presented.length > 0).nextElementSibling;
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
  // The chevron points the way the page moves, so it leads going back and
  // follows going on
  const back = Number(button.dataset.step) < 0;
  button[back ? "prepend" : "append"](icon(back ? "prev" : "next"));
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

const addForm = document.getElementById("add");
let dragDepth = 0;

function isFileDrag(event) {
  return Array.from(event.dataTransfer?.types || []).includes("Files");
}

window.addEventListener("dragenter", (event) => {
  if (!isFileDrag(event)) return;
  event.preventDefault();
  dragDepth++;
  document.body.classList.add("file-drag");
});
window.addEventListener("dragover", (event) => {
  if (isFileDrag(event)) event.preventDefault();
});
window.addEventListener("dragleave", (event) => {
  if (!isFileDrag(event)) return;
  dragDepth = Math.max(0, dragDepth - 1);
  if (!dragDepth) document.body.classList.remove("file-drag");
});
window.addEventListener("drop", (event) => {
  if (!isFileDrag(event)) return;
  event.preventDefault();
  dragDepth = 0;
  document.body.classList.remove("file-drag");
  if (event.dataTransfer.files.length) addForm.elements.file.files = event.dataTransfer.files;
});

addForm.onsubmit = async (event) => {
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

const conventions = {
  content: "sizes count decoded bytes",
  wire: "sizes count posted bytes",
  unknown: "sizes could not be read from the hints, so they are estimates",
};

// Inspect runs the same parse an add does and reports it instead of acting on
// it, so an nzb can be looked at before anything is built from it.
function renderInspect(head, target, file, data) {
  const heading = document.createElement("strong");
  setBreakable(heading, data.name || file.name);
  const summary = document.createElement("div");
  summary.className = "info-total";
  // Inspect does not probe, so an nzb whose hints identify nothing stays
  // unknown here even though adding it would settle it
  summary.textContent = `${data.files.length} posted files, ${data.segments} segments,`
    + ` ${estimated(data.bytes, data.exact)} from ${size(data.wire)} on the wire`
    + ` (${conventions[data.convention] || data.convention})`;
  summary.dataset.hint = "which size convention the numbers above use";
  head.append(heading, summary);

  for (const [kind, list] of [["error", data.errors], ["warning", data.warnings]]) {
    for (const message of list || []) {
      const line = document.createElement("div");
      line.className = "inspect-" + kind;
      line.textContent = `${kind}: ${message}`;
      target.append(line);
    }
  }

  const meta = Object.entries(data.meta || {});
  if (meta.length) {
    const table = document.createElement("table");
    table.className = "info-files inspect-meta";
    const body = table.createTBody();
    for (const [key, value] of meta.sort(([a], [b]) => collator.compare(a, b))) {
      const row = body.insertRow();
      cell(row, key);
      const shown = row.insertCell();
      // A password is over someones shoulder as easily as anything else here
      if (/password/i.test(key)) {
        const reveal = document.createElement("button");
        reveal.className = "reveal";
        reveal.append(icon("show"), "show");
        reveal.onclick = () => setBreakable(shown, value);
        shown.append(reveal);
      } else {
        setBreakable(shown, value);
      }
    }
    target.append(table);
  }

  const table = document.createElement("table");
  table.className = "info-files";
  const header = table.createTHead().insertRow();
  for (const [label, hint] of [["Posted file"], ["Size", "decoded size"],
  ["Wire", "posted size, yEnc overhead included"], ["Segments"], ["Date"]]) {
    cell(header, label, "th");
    if (hint) header.lastElementChild.dataset.hint = hint;
  }
  const body = table.createTBody();
  for (const posted of data.files.slice().sort((a, b) => byPostedName(a.filename, b.filename))) {
    const row = body.insertRow();
    // The subject, the poster and the groups are what the name was read out of:
    // under it, since they are rarely what is being looked for
    const name = nameCell(row, posted.filename || posted.subject);
    const detail = document.createElement("small");
    detail.textContent = [posted.encoding, posted.poster, (posted.groups || []).join(", ")]
      .filter(Boolean).join(" - ");
    detail.dataset.hint = posted.subject;
    name.append(detail);
    sizeCell(row, posted.bytes, posted.exact);
    sizeCell(row, posted.wire);
    // The subject says how many segments the post has; fewer listed is a gap
    cell(row, posted.segment_hint && posted.segment_hint !== posted.segments
      ? `${posted.segments} / ${posted.segment_hint}` : posted.segments);
    cell(row, new Date(posted.date).toLocaleString());
  }
  target.append(table);
}

const inspectDialog = document.getElementById("inspect-dialog");

// The backdrop reports the dialog as its target, so a click only dismisses when
// it also landed outside the box the dialog draws
inspectDialog.onclick = (event) => {
  if (event.target !== inspectDialog) return;
  const box = inspectDialog.getBoundingClientRect();
  const inside = event.clientX >= box.left && event.clientX <= box.right &&
    event.clientY >= box.top && event.clientY <= box.bottom;
  if (!inside) inspectDialog.close();
};

document.getElementById("inspect").onclick = async () => {
  const form = document.getElementById("add");
  if (!form.file.files.length) return form.reportValidity();
  const target = document.getElementById("inspect-body");
  const title = document.getElementById("inspect-title");
  target.replaceChildren();
  title.replaceChildren();
  inspectDialog.showModal();
  // The first nzb names the dialog, next to the close button; any after it are
  // headed inside the body, where their own table follows
  for (const [i, file] of [...form.file.files].entries()) {
    const body = new FormData();
    body.append("file", file);
    try {
      const response = await fetch("/api/inspect", { method: "POST", body });
      const data = await response.json();
      if (!response.ok) throw new Error(data.error);
      renderInspect(i === 0 ? title : target, target, file, data);
    } catch (err) {
      const line = document.createElement("div");
      line.className = "inspect-error";
      line.textContent = `${file.name}: ${err.message}`;
      target.append(line);
    }
  }
};

setInterval(() => { if (!document.hidden) poll(); }, 2000);
document.addEventListener("visibilitychange", () => { if (!document.hidden) poll(); });
poll();
