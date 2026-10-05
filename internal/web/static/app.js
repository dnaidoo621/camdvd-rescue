// Small behaviours htmx doesn't cover: the preview player, "all files"
// toggles in the rename picker, and toast messages from HX-Trigger.
(function () {
  "use strict";

  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest("[data-play]");
    if (!btn) return;
    ev.preventDefault();
    var dlg = document.getElementById("player");
    var video = dlg.querySelector("video");
    dlg.querySelector("h3").textContent = btn.dataset.title || "";
    dlg.querySelector(".meta").textContent = btn.dataset.meta || "";
    video.src = btn.dataset.play;
    dlg.showModal();
    video.play().catch(function () {});
  });

  document.addEventListener("close", function (ev) {
    if (ev.target.id !== "player") return;
    var video = ev.target.querySelector("video");
    video.pause();
    video.removeAttribute("src");
    video.load();
  }, true);

  document.addEventListener("change", function (ev) {
    var all = ev.target.closest("input.all[data-group]");
    if (!all) return;
    document.querySelectorAll("input." + CSS.escape(all.dataset.group)).forEach(function (cb) {
      cb.checked = all.checked;
    });
  });

  var toastTimer;
  function toast(msg) {
    var el = document.getElementById("toast");
    if (!el || !msg) return;
    el.textContent = msg;
    el.classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el.classList.remove("show"); }, 4000);
  }
  document.body.addEventListener("toast", function (ev) {
    toast(typeof ev.detail === "string" ? ev.detail : ev.detail && ev.detail.value);
  });
  // Drive and job notices also arrive as SSE "notice" events.
  document.body.addEventListener("htmx:sseMessage", function (ev) {
    if (ev.detail && ev.detail.type === "notice") {
      try { toast(JSON.parse(ev.detail.data)); } catch (e) { toast(ev.detail.data); }
    }
  });

  // Live updates redraw job cards several times a second while a disc is
  // read. Keep what's being typed into an answers form, and the cursor,
  // across each redraw.
  var kept = [];
  document.body.addEventListener("htmx:beforeSwap", function (ev) {
    kept = [];
    var target = ev.detail.target;
    if (!target || !target.querySelectorAll) return;
    var forms = Array.prototype.slice.call(target.querySelectorAll("form.answers"));
    if (target.matches && target.matches("form.answers")) forms.push(target);
    forms.forEach(function (f) {
      if (f === ev.detail.requestConfig.elt) return; // the form's own submit
      var desc = f.querySelector("input[name=description]");
      var side = f.querySelector("input[name=sides]:checked");
      var focused = document.activeElement === desc;
      kept.push({
        id: f.id, desc: desc ? desc.value : "", side: side ? side.value : null, focused: focused,
        start: focused ? desc.selectionStart : null, end: focused ? desc.selectionEnd : null
      });
    });
  });
  document.body.addEventListener("htmx:afterSwap", function () {
    kept.forEach(function (k) {
      var f = document.getElementById(k.id);
      if (!f) return;
      var desc = f.querySelector("input[name=description]");
      if (desc) desc.value = k.desc;
      if (k.side) {
        var r = f.querySelector("input[name=sides][value='" + k.side + "']");
        if (r) r.checked = true;
      }
      if (k.focused && desc) {
        desc.focus({ preventScroll: true });
        try { desc.setSelectionRange(k.start, k.end); } catch (e) {}
      }
    });
    kept = [];
  });

  // A rename preview on page load when a folder is preselected.
  document.addEventListener("DOMContentLoaded", function () {
    var form = document.querySelector("form.rename");
    if (form && form.querySelector("input[name=clip]:checked")) htmx.trigger(form, "submit");
  });
})();
