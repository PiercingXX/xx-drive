/* xx-drive SPA — vanilla JS, no dependencies.
   Layout: helpers → state → boot/auth → views → file ops → uploads → dialogs → preview → events wiring */
"use strict";

(() => {
  // ---------- tiny helpers ----------
  const $ = (id) => document.getElementById(id);
  const el = (tag, attrs = {}, ...kids) => {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") n.className = v;
      else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
      else if (v !== null && v !== undefined) n.setAttribute(k, v);
    }
    for (const kid of kids) if (kid != null) n.append(kid);
    return n;
  };
  const fmtSize = (n) => {
    if (n == null) return "";
    if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + " GB";
    if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
    if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + " KB";
    return n + " B";
  };
  const fmtDate = (unix) => {
    if (!unix) return "";
    const d = new Date(unix * 1000), now = Date.now();
    const diff = (now - d.getTime()) / 1000;
    if (diff < 60) return "just now";
    if (diff < 3600) return Math.floor(diff / 60) + " min ago";
    if (diff < 86400) return Math.floor(diff / 3600) + " h ago";
    if (diff < 7 * 86400) return Math.floor(diff / 86400) + " d ago";
    return d.toLocaleDateString();
  };
  const extOf = (name) => {
    const i = name.lastIndexOf(".");
    return i < 0 ? "" : name.slice(i).toLowerCase();
  };
  const iconFor = (e) => {
    if (e.isDir) return "\uD83D\uDCC1";
    switch (extOf(e.name)) {
      case ".jpg": case ".jpeg": case ".png": case ".gif": case ".webp": case ".svg": case ".avif":
        return "\uD83D\uDDBC";
      case ".mp4": case ".webm": case ".mov": case ".mkv":
        return "\uD83C\uDFAC";
      case ".mp3": case ".flac": case ".ogg": case ".wav":
        return "\uD83C\uDFB5";
      case ".pdf": return "\uD83D\uDCC4";
      case ".zip": case ".tar": case ".gz": case ".7z": case ".rar": return "\uD83D\uDCDC";
      case ".txt": case ".md": return "\uD83D\uDCC4";
      default: return "\uD83D\uDCC6";
    }
  };

  async function api(path, opts = {}) {
    const o = { headers: {}, ...opts };
    if (o.json !== undefined) {
      o.body = JSON.stringify(o.json);
      o.headers["Content-Type"] = "application/json";
      delete o.json;
    }
    const resp = await fetch(path, o);
    let data = null;
    try { data = await resp.json(); } catch { /* non-JSON */ }
    if (!resp.ok) {
      const msg = (data && data.error) || ("HTTP " + resp.status);
      const err = new Error(msg);
      err.status = resp.status;
      throw err;
    }
    return data;
  }

  function toast(msg, kind = "") {
    const t = el("div", { class: "toast " + kind }, String(msg));
    $("toasts").append(t);
    setTimeout(() => t.remove(), kind === "err" ? 6000 : 3200);
  }

  // ---------- state ----------
  const S = {
    view: "files",
    path: "/",
    entries: [],
    selection: new Set(),   // paths
    sort: { key: "name", dir: 1 },
    grid: false,
    me: null,
    searchMode: false,
    trashItems: [],
  };

  // ---------- boot & auth ----------
  async function boot() {
    try {
      S.me = await api("/api/auth/me");
      enterApp();
    } catch {
      showLogin();
    }
    if ("serviceWorker" in navigator) {
      navigator.serviceWorker.register("/sw.js").catch(() => {});
    }
  }

  function showLogin() {
    $("loginView").style.display = "flex";
    $("appView").classList.remove("on");
    $("liUser").focus();
  }

  function enterApp() {
    $("loginView").style.display = "none";
    $("appView").classList.add("on");
    $("whoami").textContent = S.me.username + (S.me.isAdmin ? " · admin" : "");
    $("navAdmin").style.display = S.me.isAdmin ? "" : "none";
    setView("files");
  }

  $("loginForm").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    $("loginErr").textContent = "";
    try {
      const r = await api("/api/auth/login", { method: "POST", json: {
        username: $("liUser").value.trim(), password: $("liPass").value } });
      S.me = r.user;
      enterApp();
    } catch (e) {
      $("loginErr").textContent = e.message;
    }
  });

  $("logoutBtn").addEventListener("click", async () => {
    try { await api("/api/auth/logout", { method: "POST" }); } catch {}
    location.reload();
  });

  // ---------- navigation ----------
  document.querySelectorAll("#nav button").forEach((b) =>
    b.addEventListener("click", () => setView(b.dataset.view)));

  $("hamburger").addEventListener("click", () => $("rail").classList.toggle("open"));

  function setView(v) {
    S.view = v;
    S.selection.clear();
    document.querySelectorAll("#nav button").forEach((b) =>
      b.classList.toggle("active", b.dataset.view === v));
    const isFilesish = v === "files" || v === "starred" || v === "search";
    $("filesToolbar").style.display = v === "files" ? "" : "none";
    $("crumbs").style.visibility = v === "files" ? "visible" : "hidden";
    if (v === "files") renderFiles();
    else if (v === "starred") renderStarred();
    else if (v === "shares") renderShares();
    else if (v === "trash") renderTrash();
    else if (v === "events") renderEvents();
    else if (v === "admin") renderAdmin();
    $("rail").classList.remove("open");
  }

  // ---------- breadcrumbs ----------
  function renderCrumbs(path) {
    const c = $("crumbs");
    c.innerHTML = "";
    c.append(el("button", { onclick: () => navTo("/") }, "My Files"));
    if (path && path !== "/") {
      const parts = path.split("/").filter(Boolean);
      let acc = "";
      parts.forEach((p, i) => {
        acc += "/" + p;
        if (i === parts.length - 1) c.append(el("span", { class: "sep" }, "›"), el("span", { class: "cur" }, p));
        else c.append(el("span", { class: "sep" }, "›"), el("button", { onclick: () => navTo(acc) }, p));
      });
    }
  }

  async function navTo(path) {
    S.path = path;
    S.searchMode = false;
    await renderFiles();
  }

  // ---------- sorting ----------
  function sortedEntries(list) {
    const { key, dir } = S.sort;
    return [...list].sort((a, b) => {
      if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
      let r = 0;
      if (key === "size") r = (a.size || 0) - (b.size || 0);
      else if (key === "mtime") r = (a.mtime || 0) - (b.mtime || 0);
      else r = a.name.toLowerCase().localeCompare(b.name.toLowerCase());
      return r * dir;
    });
  }

  // ---------- file browser ----------
  async function renderFiles() {
    const area = $("browserArea");
    try {
      const r = await api("/api/files/list?path=" + encodeURIComponent(S.path));
      S.entries = r.entries;
      S.searchMode = false;
    } catch (e) {
      toast(e.message, "err");
      if (e.status === 404) { S.path = "/"; return renderFiles(); }
      return;
    }
    renderCrumbs(S.path);
    drawEntries(sortedEntries(S.entries));
  }

  function drawEntries(list) {
    const area = $("browserArea");
    area.innerHTML = "";
    if (!list.length) {
      area.append(el("div", { class: "empty-state" }, "Nothing here yet — drop files to upload."));
      updateSelbar();
      return;
    }
    if (S.grid && !S.searchMode) {
      const g = el("div", { class: "grid" });
      for (const e of list) g.append(gridCell(e));
      area.append(g);
    } else {
      area.append(fileTable(list));
    }
    updateSelbar();
  }

  function starToggle(ev, e) {
    ev.stopPropagation();
    api("/api/star/toggle", { method: "POST", json: { path: e.path } })
      .then((r) => { e.starred = r.starred; refreshCurrent(); })
      .catch((err) => toast(err.message, "err"));
  }

  function rowClick(ev, e) {
    if (ev.ctrlKey || ev.metaKey) toggleSel(e.path);
    else { S.selection.clear(); toggleSel(e.path); }
    drawSelection();
  }

  function rowOpen(e) {
    if (e.isDir) navTo(e.path);
    else openPreview(e);
  }

  function toggleSel(p) {
    if (S.selection.has(p)) S.selection.delete(p); else S.selection.add(p);
  }

  function drawSelection() {
    document.querySelectorAll("tr.row, .cell").forEach((n) => {
      const p = n.dataset.path;
      n.classList.toggle("selected", S.selection.has(p));
    });
    updateSelbar();
  }

  function selectedEntries() {
    return S.entries.filter((e) => S.selection.has(e.path));
  }

  function updateSelbar() {
    const bar = $("selbar");
    const n = S.selection.size;
    bar.classList.toggle("on", n > 0);
    $("selCount").textContent = n ? n + " selected" : "";
  }

  function fileTable(list) {
    const t = el("table", { class: "files" });
    const mkTh = (label, key) =>
      el("th", { onclick: () => { setSort(key); } },
        label + (S.sort.key === key ? (S.sort.dir > 0 ? " ▲" : " ▼") : ""));
    const trh = el("tr", {},
      el("th", { style: "width:34px" }),
      mkTh("Name", "name"),
      el("th", { style: "width:110px" }, "Size"),
      mkTh("Modified", "mtime"),
      el("th", { style: "width:60px" }));
    t.append(el("thead", {}, trh));
    const tb = el("tbody");
    for (const e of list) {
      const name = el("div", { class: "fname" },
        el("span", { class: "ico" }, iconFor(e)),
        el("span", { class: "nm" }, e.name));
      name.addEventListener("click", (ev) => rowClick(ev, e));
      name.addEventListener("dblclick", () => rowOpen(e));
      const tr = el("tr", { class: "row", "data-path": e.path },
        el("td", {}, checkbox(e)),
        el("td", {}, name),
        el("td", { class: "dim" }, e.isDir ? "—" : fmtSize(e.size)),
        el("td", { class: "dim" }, fmtDate(e.mtime)),
        el("td", {}, el("button", {
          class: "star-btn" + (e.starred ? " on" : ""),
          title: "Star",
          onclick: (ev) => starToggle(ev, e),
        }, e.starred ? "★" : "☆")));
      tr.addEventListener("contextmenu", (ev) => { ev.preventDefault(); openCtx(ev, e); });
      tb.append(tr);
    }
    t.append(tb);
    return t;
  }

  function checkbox(e) {
    const cb = el("input", { type: "checkbox" });
    cb.checked = S.selection.has(e.path);
    cb.addEventListener("click", (ev) => { ev.stopPropagation(); toggleSel(e.path); drawSelection(); });
    return cb;
  }

  function gridCell(e) {
    const c = el("div", { class: "cell", "data-path": e.path },
      el("div", { class: "big" }, iconFor(e)),
      el("div", {}, e.name),
      el("div", { class: "meta" }, e.isDir ? "folder" : fmtSize(e.size)));
    c.addEventListener("click", (ev) => rowClick(ev, e));
    c.addEventListener("dblclick", () => rowOpen(e));
    c.addEventListener("contextmenu", (ev) => { ev.preventDefault(); openCtx(ev, e); });
    return c;
  }

  function setSort(key) {
    if (S.sort.key === key) S.sort.dir *= -1;
    else S.sort = { key, dir: 1 };
    drawEntries(sortedEntries(S.entries));
  }

  $("viewToggleBtn").addEventListener("click", () => {
    S.grid = !S.grid;
    $("viewToggleBtn").textContent = S.grid ? "List" : "Grid";
    drawEntries(sortedEntries(S.entries));
  });

  // ---------- selection actions ----------
  document.querySelectorAll("#selbar button[data-act]").forEach((b) =>
    b.addEventListener("click", () => actOnSelection(b.dataset.act)));

  function actOnSelection(act) {
    const sel = selectedEntries();
    if (act === "clearsel") { S.selection.clear(); drawSelection(); return; }
    if (!sel.length) return;
    if (act === "delete") {
      confirmModal(`Delete ${sel.length} item(s) to trash?`, async () => {
        for (const e of sel) {
          try { await api("/api/files/delete", { method: "POST", json: { path: e.path } }); }
          catch (err) { toast(err.message, "err"); }
        }
        S.selection.clear();
        toast("Moved to trash", "ok");
        refreshCurrent();
      });
    } else if (act === "download") {
      for (const e of sel) {
        if (e.isDir) window.open("/api/files/zip?path=" + encodeURIComponent(e.path), "_blank");
        else window.open("/api/files/download?path=" + encodeURIComponent(e.path), "_blank");
      }
    } else if (act === "move") {
      destDialog("Move " + sel.length + " item(s) to…", S.path, async (destDir) => {
        for (const e of sel) {
          try { await api("/api/files/move", { method: "POST", json: { path: e.path, destDir } }); }
          catch (err) { toast(err.message, "err"); }
        }
        toast("Moved", "ok"); S.selection.clear(); refreshCurrent();
      });
    } else if (act === "copy") {
      destDialog("Copy " + sel.length + " item(s) to…", S.path, async (destDir) => {
        for (const e of sel) {
          try { await api("/api/files/copy", { method: "POST", json: { path: e.path, destDir } }); }
          catch (err) { toast(err.message, "err"); }
        }
        toast("Copied", "ok"); S.selection.clear(); refreshCurrent();
      });
    } else if (act === "share" && sel.length === 1) {
      shareDialog(sel[0]);
    }
  }

  // ---------- context menu ----------
  let ctxEntry = null;
  function openCtx(ev, e) {
    ctxEntry = e;
    if (!S.selection.has(e.path)) { S.selection.clear(); S.selection.add(e.path); drawSelection(); }
    const m = $("ctxMenu");
    m.innerHTML = "";
    const add = (label, fn, cls = "") => {
      m.append(el("button", { class: cls, onclick: () => { m.classList.remove("on"); fn(); } }, label));
    };
    if (e.isDir) add("Open", () => navTo(e.path));
    else add("Preview", () => openPreview(e));
    add("Download" + (e.isDir ? " (.zip)" : ""), () => {
      const u = e.isDir ? "/api/files/zip?path=" : "/api/files/download?path=";
      window.open(u + encodeURIComponent(e.path), "_blank");
    });
    add(e.starred ? "Remove star" : "Add star", () => starToggle(new Event("x"), e));
    add("Share…", () => shareDialog(e));
    add("Rename…", () => renameDialog(e));
    add("Move…", () => destDialog("Move “" + e.name + "” to…", S.path, (d) =>
      api("/api/files/move", { method: "POST", json: { path: e.path, destDir: d } })
        .then(() => { toast("Moved", "ok"); refreshCurrent(); })
        .catch((err) => toast(err.message, "err"))));
    add("Copy…", () => destDialog("Copy “" + e.name + "” to…", S.path, (d) =>
      api("/api/files/copy", { method: "POST", json: { path: e.path, destDir: d } })
        .then(() => { toast("Copied", "ok"); refreshCurrent(); })
        .catch((err) => toast(err.message, "err"))));
    add("Versions…", () => versionsDialog(e));
    add("Delete", () => actOnSelection("delete"), "danger");

    m.style.left = Math.min(ev.clientX, innerWidth - 190) + "px";
    m.style.top = Math.min(ev.clientY, innerHeight - 380) + "px";
    m.classList.add("on");
  }
  document.addEventListener("click", (ev) => {
    if (!ev.target.closest("#ctxMenu")) $("ctxMenu").classList.remove("on");
  });

  // ---------- toolbar ----------
  $("newFolderBtn").addEventListener("click", () => {
    textModal("New folder", "Folder name", "", async (name) => {
      const p = (S.path === "/" ? "" : S.path) + "/" + name;
      await api("/api/files/mkdir", { method: "POST", json: { path: p } });
      toast("Folder created", "ok");
      refreshCurrent();
    });
  });

  $("uploadBtn").addEventListener("click", () => $("fileInput").click());
  $("fileInput").addEventListener("change", () => {
    queueUploads([...$("fileInput").files]);
    $("fileInput").value = "";
  });

  // ---------- drag & drop ----------
  const content = $("content");
  let dragDepth = 0;
  content.addEventListener("dragenter", (ev) => {
    ev.preventDefault();
    if (++dragDepth === 1) $("dropOverlay").classList.add("on");
  });
  content.addEventListener("dragleave", () => {
    if (--dragDepth <= 0) { dragDepth = 0; $("dropOverlay").classList.remove("on"); }
  });
  content.addEventListener("dragover", (ev) => ev.preventDefault());
  content.addEventListener("drop", (ev) => {
    ev.preventDefault();
    dragDepth = 0;
    $("dropOverlay").classList.remove("on");
    queueUploads([...ev.dataTransfer.files]);
  });

  // ---------- uploads ----------
  function queueUploads(files) {
    if (!files.length) return;
    $("uploadsPanel").classList.add("on");
    for (const f of files) uploadOne(f);
  }

  function uploadOne(file) {
    // target path = current dir + filename; conflict=rename keeps both copies
    const target = (S.path === "/" ? "" : S.path) + "/" + file.name;
    const item = el("div", { class: "up-item" },
      el("div", { class: "row1" },
        el("span", {}, file.name),
        el("button", { class: "x", title: "Cancel" }, "✕")),
      el("div", { class: "bar" }, el("i")));
    $("uploadsList").prepend(item);
    const xhr = new XMLHttpRequest();
    item._xhr = xhr;
    item.querySelector(".x").addEventListener("click", () => xhr.abort());
    const bar = item.querySelector(".bar i");
    xhr.upload.addEventListener("progress", (ev) => {
      if (ev.lengthComputable) bar.style.width = (ev.loaded / ev.total) * 100 + "%";
    });
    xhr.addEventListener("load", () => {
      if (xhr.status === 200) {
        let r = {};
        try { r = JSON.parse(xhr.responseText); } catch {}
        item.classList.add("done");
        bar.style.width = "100%";
        if (r.conflicted) toast("Conflict copy created for " + file.name);
        setTimeout(refreshCurrent, 400);
      } else {
        item.classList.add("fail");
        let r = {}; try { r = JSON.parse(xhr.responseText); } catch {}
        toast(file.name + ": " + (r.error || ("HTTP " + xhr.status)), "err");
      }
      maybeHideUploads();
    });
    xhr.addEventListener("error", () => { item.classList.add("fail"); maybeHideUploads(); });
    xhr.addEventListener("abort", () => { item.remove(); maybeHideUploads(); });
    xhr.open("POST", "/api/files/upload?path=" + encodeURIComponent(target) + "&conflict=rename");
    const fd = new FormData();
    fd.append("file", file, file.name);
    xhr.send(fd);
  }

  function maybeHideUploads() {
    setTimeout(() => {
      if (!$("uploadsList").children.length) $("uploadsPanel").classList.remove("on");
    }, 1500);
  }
  $("uploadsClose").addEventListener("click", () => $("uploadsPanel").classList.remove("on"));

  // ---------- search ----------
  let searchTimer = null;
  $("searchInput").addEventListener("input", () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(runSearch, 350);
  });
  $("searchInput").addEventListener("keydown", (ev) => {
    if (ev.key === "Enter") runSearch();
  });
  $("searchBtn").addEventListener("click", runSearch);

  async function runSearch() {
    const q = $("searchInput").value.trim();
    if (!q) { setView("files"); return; }
    try {
      const results = await api("/api/search?q=" + encodeURIComponent(q));
      S.view = "search";
      document.querySelectorAll("#nav button").forEach((b) => b.classList.remove("active"));
      renderCrumbs("");
      const area = $("browserArea");
      area.innerHTML = "";
      area.append(el("h3", { style: "margin:4px 0 12px" }, `Results for “${q}” (${results.length})`));
      if (!results.length) area.append(el("div", { class: "empty-state" }, "No matches."));
      for (const e of results) {
        const row = el("div", { class: "share-row" },
          el("span", {}, iconFor(e)),
          el("a", { href: "#", onclick: (ev) => { ev.preventDefault(); navTo(parentOf(e.path)); } },
            parentOf(e.path) === "/" ? e.path : e.path),
          el("span", { class: "dim", style: "margin-left:auto" }, e.isDir ? "folder" : fmtSize(e.size)));
        area.append(row);
      }
    } catch (e) { toast(e.message, "err"); }
  }

  const parentOf = (p) => {
    const i = p.lastIndexOf("/");
    return i <= 0 ? "/" : p.slice(0, i);
  };

  // ---------- starred ----------
  async function renderStarred() {
    renderCrumbs("");
    const area = $("browserArea");
    area.innerHTML = "";
    try {
      const items = await api("/api/starred");
      if (!items.length) { area.append(el("div", { class: "empty-state" }, "No starred items.")); return; }
      S.entries = items;
      drawEntries(items);
    } catch (e) { toast(e.message, "err"); }
  }

  // ---------- shares view ----------
  async function renderShares() {
    renderCrumbs("");
    const area = $("browserArea");
    area.innerHTML = "";
    let shares = [];
    try { shares = await api("/api/shares"); } catch (e) { toast(e.message, "err"); return; }
    if (!shares.length) { area.append(el("div", { class: "empty-state" }, "No shared links yet. Share a file from My Files.")); return; }
    const t = el("table", { class: "plain" },
      el("thead", {}, el("tr", {},
        el("th", {}, "Path"), el("th", {}, "Created"), el("th", {}, "Expires"),
        el("th", {}, "Password"), el("th", {}, "Download"), el("th", {}, ""))));
    const tb = el("tbody");
    for (const sh of shares) {
      tb.append(el("tr", {},
        el("td", {}, sh.path),
        el("td", { class: "dim" }, fmtDate(sh.createdAt)),
        el("td", { class: "dim" }, sh.expiresAt ? new Date(sh.expiresAt * 1000).toLocaleDateString() : "never"),
        el("td", { class: "dim" }, sh.hasPassword ? "yes" : "no"),
        el("td", { class: "dim" }, sh.allowDownload ? "allowed" : "view only"),
        el("td", {},
          el("button", {
            class: "small", onclick: () => copyText(location.origin + sh.url),
          }, "Copy link"),
          " ",
          el("button", {
            class: "small danger", onclick: async () => {
              try { await api("/api/shares/" + sh.tokenHash, { method: "DELETE" }); toast("Revoked", "ok"); renderShares(); }
              catch (e) { toast(e.message, "err"); }
            },
          }, "Revoke"))));
    }
    t.append(tb);
    area.append(t);
  }

  function copyText(text) {
    navigator.clipboard.writeText(text)
      .then(() => toast("Link copied", "ok"))
      .catch(() => textModal("Copy link", "Link", text, () => {}));
  }

  // ---------- trash ----------
  async function renderTrash() {
    renderCrumbs("");
    const area = $("browserArea");
    area.innerHTML = "";
    try { S.trashItems = await api("/api/trash"); } catch (e) { toast(e.message, "err"); return; }
    const head = el("div", { class: "toolbar" },
      el("button", {
        class: "danger", onclick: () => confirmModal("Permanently empty the trash?", async () => {
          await api("/api/trash/empty", { method: "POST" });
          toast("Trash emptied", "ok"); renderTrash();
        }),
      }, "Empty trash"));
    area.append(head);
    if (!S.trashItems.length) { area.append(el("div", { class: "empty-state" }, "Trash is empty.")); return; }
    const t = el("table", { class: "plain" },
      el("thead", {}, el("tr", {},
        el("th", {}, "Name"), el("th", {}, "Original location"), el("th", {}, "Deleted"), el("th", {}, ""))));
    const tb = el("tbody");
    for (const it of S.trashItems) {
      tb.append(el("tr", {},
        el("td", {}, (it.isDir ? "\uD83D\uDCC1 " : "\uD83D\uDCC4 ") + it.name),
        el("td", { class: "dim" }, it.origPath),
        el("td", { class: "dim" }, fmtDate(it.deletedAt)),
        el("td", {},
          el("button", {
            class: "small", onclick: async () => {
              try { await api("/api/trash/restore", { method: "POST", json: { id: it.id } }); toast("Restored", "ok"); renderTrash(); }
              catch (e) { toast(e.message, "err"); }
            },
          }, "Restore"),
          " ",
          el("button", {
            class: "small danger", onclick: () => confirmModal("Permanently delete “" + it.name + "”?", async () => {
              try { await api("/api/trash/delete", { method: "POST", json: { id: it.id } }); renderTrash(); }
              catch (e) { toast(e.message, "err"); }
            }),
          }, "Delete forever"))));
    }
    t.append(tb);
    area.append(t);
  }

  // ---------- events ----------
  async function renderEvents() {
    renderCrumbs("");
    const area = $("browserArea");
    area.innerHTML = "";
    let evs = [];
    try { evs = await api("/api/events?limit=100"); } catch (e) { toast(e.message, "err"); return; }
    if (!evs.length) { area.append(el("div", { class: "empty-state" }, "No activity yet.")); return; }
    const icons = { upload: "⬆", download: "⬇", delete: "🗑", restore: "♻", mkdir: "📁", rename: "✏", move: "📦", copy: "❐", share_create: "🔗", share_revoke: "⛓", login: "🔑", version_restore: "⏪", purge: "🔥", empty_trash: "🧹" };
    const t = el("table", { class: "plain" });
    const tb = el("tbody");
    for (const ev of evs) {
      tb.append(el("tr", {},
        el("td", {}, el("span", { class: "ev-kind" }, (icons[ev.kind] || "•") + " " + ev.kind)),
        el("td", {}, ev.detail),
        el("td", { class: "dim", style: "text-align:right;white-space:nowrap" }, fmtDate(ev.createdAt))));
    }
    t.append(tb);
    area.append(t);
  }

  // ---------- admin ----------
  async function renderAdmin() {
    renderCrumbs("");
    const area = $("browserArea");
    area.innerHTML = "";
    let users = [];
    try { users = await api("/api/admin/users"); } catch (e) { toast(e.message, "err"); return; }

    const box = el("div", { class: "modal", style: "max-width:640px;margin:0 auto" });
    box.append(el("h3", {}, "Users"));
    const form = el("div", { class: "row" },
      Object.assign(el("input", { type: "text", placeholder: "username", id: "nuName" }), { style: "flex:1" }),
      Object.assign(el("input", { type: "password", placeholder: "password (min 8)", id: "nuPass" }), { style: "flex:1" }),
      el("label", { class: "row", style: "margin:0" }, el("input", { type: "checkbox", id: "nuAdmin" }), "admin"),
      el("button", {
        class: "primary small", onclick: async () => {
          try {
            await api("/api/admin/users", { method: "POST", json: {
              username: $("nuName").value.trim(), password: $("nuPass").value, isAdmin: $("nuAdmin").checked } });
            toast("User created", "ok"); renderAdmin();
          } catch (e) { toast(e.message, "err"); }
        },
      }, "Create"));
    box.append(form);

    const t = el("table", { class: "plain", style: "margin-top:14px" },
      el("thead", {}, el("tr", {},
        el("th", {}, "Username"), el("th", {}, "Role"), el("th", {}, "Status"), el("th", {}, ""))));
    const tb = el("tbody");
    for (const u of users) {
      tb.append(el("tr", {},
        el("td", {}, u.username),
        el("td", { class: "dim" }, u.isAdmin ? "admin" : "user"),
        el("td", { class: "dim" }, u.disabled ? "disabled" : "active"),
        el("td", {},
          el("button", {
            class: "small", onclick: async () => {
              try {
                await api("/api/admin/users/set-state", { method: "POST", json: { username: u.username, disabled: !u.disabled } });
                renderAdmin();
              } catch (e) { toast(e.message, "err"); }
            },
          }, u.disabled ? "Enable" : "Disable"),
          " ",
          el("button", {
            class: "small", onclick: () => textModal("Reset password — " + u.username, "New password", "", async (pw) => {
              try {
                await api("/api/admin/users/password", { method: "POST", json: { username: u.username, password: pw } });
                toast("Password updated", "ok");
              } catch (e) { toast(e.message, "err"); }
            }),
          }, "Set password"))));
    }
    t.append(tb);
    box.append(t);
    area.append(box);
  }

  // ---------- modals ----------
  function showModal(node) {
    const box = $("modalBox");
    box.innerHTML = "";
    box.append(node);
    $("modalBack").classList.add("on");
  }
  function closeModal() { $("modalBack").classList.remove("on"); }
  $("modalBack").addEventListener("click", (ev) => {
    if (ev.target === $("modalBack")) closeModal();
  });

  function textModal(title, label, value, onSubmit) {
    const inp = el("input", { type: "text", value: value || "" });
    const err = el("div", { class: "err-msg" });
    const form = el("form", {},
      el("h3", {}, title),
      el("label", {}, label), inp, err,
      el("div", { class: "actions" },
        el("button", { type: "button", onclick: closeModal }, "Cancel"),
        el("button", { class: "primary", type: "submit" }, "OK")));
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      try { await onSubmit(inp.value.trim()); closeModal(); }
      catch (e) { err.textContent = e.message; }
    });
    showModal(form);
    inp.focus();
  }

  function confirmModal(msg, onYes) {
    showModal(el("div", {},
      el("h3", {}, "Confirm"),
      el("p", {}, msg),
      el("div", { class: "actions" },
        el("button", { onclick: closeModal }, "Cancel"),
        el("button", {
          class: "danger", onclick: async () => { closeModal(); await onYes(); },
        }, "Confirm"))));
  }

  function renameDialog(e) {
    textModal("Rename", "New name", e.name, async (newName) => {
      if (!newName || newName === e.name) return;
      await api("/api/files/rename", { method: "POST", json: { path: e.path, newName } });
      toast("Renamed", "ok");
      refreshCurrent();
    });
  }

  function destDialog(title, defaultDir, onOk) {
    const inp = el("input", { type: "text", value: defaultDir });
    const hint = el("div", { class: "dim", style: "margin-top:8px;font-size:12px" }, "Tip: type a folder path starting with /");
    showModal(el("form", {},
      el("h3", {}, title),
      el("label", {}, "Destination folder"), inp, hint,
      el("div", { class: "actions" },
        el("button", { type: "button", onclick: closeModal }, "Cancel"),
        el("button", { class: "primary", type: "submit" }, "Go"))));
    const form = $("modalBox").querySelector("form");
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      try { await onOk(inp.value.trim() || "/"); closeModal(); }
      catch (e) { toast(e.message, "err"); }
    });
  }

  // ---------- share dialog ----------
  function shareDialog(e) {
    const pwInp = el("input", { type: "password", placeholder: "none" });
    const daysInp = el("input", { type: "number", min: "0", placeholder: "never" });
    const dlChk = el("input", { type: "checkbox" });
    dlChk.checked = true;
    const linkBox = el("div");
    const err = el("div", { class: "err-msg" });

    const create = el("button", {
      class: "primary", type: "button", onclick: async () => {
        err.textContent = "";
        try {
          const r = await api("/api/shares", { method: "POST", json: {
            path: e.path,
            password: pwInp.value || undefined,
            expiresInDays: daysInp.value ? parseInt(daysInp.value, 10) : undefined,
            allowDownload: dlChk.checked,
          } });
          const url = location.origin + "/s/" + r.token;
          linkBox.innerHTML = "";
          linkBox.append(el("div", { class: "share-row" },
            el("a", { href: url, target: "_blank" }, url),
            el("button", { class: "small", onclick: () => copyText(url) }, "Copy")));
          toast("Share link created", "ok");
        } catch (ex) { err.textContent = ex.message; }
      },
    }, "Create link");

    showModal(el("div", {},
      el("h3", {}, "Share “" + e.name + "”"),
      el("label", {}, "Password (optional)"), pwInp,
      el("label", {}, "Expires in days (optional)"), daysInp,
      el("div", { class: "row" }, dlChk, el("span", {}, "Allow download")),
      el("div", { class: "actions", style: "justify-content:flex-start" }, create),
      err, linkBox,
      el("div", { style: "margin-top:14px" }, existingSharesFor(e.path)),
      el("div", { class: "actions" }, el("button", { onclick: closeModal }, "Close"))));

    function existingSharesFor(path) {
      const wrap = el("div");
      api("/api/shares").then((shares) => {
        const mine = shares.filter((s) => s.path === path);
        if (!mine.length) { wrap.append(el("div", { class: "dim", style: "font-size:12px" }, "No existing links for this item.")); return; }
        for (const s of mine) {
          wrap.append(el("div", { class: "share-row" },
            el("span", {}, location.origin + s.url),
            el("button", { class: "small", onclick: () => copyText(location.origin + s.url) }, "Copy"),
            el("button", {
              class: "small danger", onclick: async () => {
                try { await api("/api/shares/" + s.tokenHash, { method: "DELETE" }); toast("Revoked", "ok"); closeModal(); shareDialog(e); }
                catch (ex) { toast(ex.message, "err"); }
              },
            }, "Revoke")));
        }
      }).catch(() => {});
      return wrap;
    }
  }

  // ---------- versions dialog ----------
  function versionsDialog(e) {
    const box = el("div", {}, el("h3", {}, "Versions — " + e.name));
    const listWrap = el("div");
    box.append(listWrap,
      el("div", { class: "actions" }, el("button", { onclick: closeModal }, "Close")));
    api("/api/versions?path=" + encodeURIComponent(e.path)).then((vers) => {
      if (!vers.length) { listWrap.append(el("div", { class: "dim" }, "No previous versions yet. Overwrite the file to create history.")); return; }
      for (const v of vers) {
        listWrap.append(el("div", { class: "ver-row" },
          el("span", { style: "flex:1" }, new Date(v.createdAt * 1000).toLocaleString()),
          el("span", { class: "dim" }, fmtSize(v.size)),
          el("button", {
            class: "small", onclick: () => window.open(
              "/api/versions/download?path=" + encodeURIComponent(e.path) +
              "&versionId=" + encodeURIComponent(v.versionId), "_blank"),
          }, "Download"),
          el("button", {
            class: "small", onclick: () => confirmModal("Restore this version? Current content will be kept as a new version.", async () => {
              try {
                await api("/api/versions/restore", { method: "POST", json: { path: e.path, versionId: v.versionId } });
                toast("Version restored", "ok"); closeModal(); refreshCurrent();
              } catch (ex) { toast(ex.message, "err"); }
            }),
          }, "Restore")));
      }
    }).catch((ex) => toast(ex.message, "err"));
    showModal(box);
  }

  // ---------- preview ----------
  const IMG_EXT = [".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".bmp", ".svg"];
  const VID_EXT = [".mp4", ".webm", ".mov", ".m4v"];
  const TEXT_EXT = [".txt", ".md", ".json", ".js", ".css", ".html", ".xml", ".yml", ".yaml", ".log", ".csv", ".go", ".py", ".sh"];

  async function openPreview(e) {
    const ext = extOf(e.name);
    const url = "/api/files/download?path=" + encodeURIComponent(e.path) + "&inline=1";
    if (IMG_EXT.includes(ext)) {
      const lb = $("lightbox");
      lb.innerHTML = "";
      const img = el("img");
      img.src = URL.createObjectURL(await (await fetch(url)).blob());
      lb.append(img);
      lb.classList.add("on");
    } else if (VID_EXT.includes(ext)) {
      const lb = $("lightbox");
      lb.innerHTML = "";
      const vid = el("video", { controls: "", autoplay: "" });
      vid.src = url;
      lb.append(vid);
      lb.classList.add("on");
    } else if (ext === ".pdf") {
      showModal(el("div", { class: "preview-box" },
        el("h3", {}, e.name),
        el("iframe", { src: url }),
        el("div", { class: "actions" }, el("button", { onclick: closeModal }, "Close"))));
    } else if (TEXT_EXT.includes(ext) && (e.size || 0) < 2 << 20) {
      const text = await (await fetch(url)).text();
      showModal(el("div", { class: "preview-box" },
        el("h3", {}, e.name),
        el("pre", {}, text),
        el("div", { class: "actions" },
          el("button", { onclick: () => window.open(url, "_blank") }, "Raw"),
          el("button", { onclick: closeModal }, "Close"))));
    } else {
      window.open("/api/files/download?path=" + encodeURIComponent(e.path), "_blank");
    }
  }

  $("lightbox").addEventListener("click", () => {
    $("lightbox").classList.remove("on");
    $("lightbox").innerHTML = "";
  });

  // ---------- keyboard ----------
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") {
      $("ctxMenu").classList.remove("on");
      $("lightbox").classList.remove("on");
      $("lightbox").innerHTML = "";
      closeModal();
      S.selection.clear();
      drawSelection();
    }
    if ((ev.key === "Delete") && S.selection.size && S.view === "files"
        && !ev.target.closest("input, textarea")) {
      actOnSelection("delete");
    }
  });

  function refreshCurrent() {
    if (S.view === "files") renderFiles();
    else if (S.view === "starred") renderStarred();
    else if (S.view === "trash") renderTrash();
    else if (S.view === "shares") renderShares();
  }

  boot();
})();
