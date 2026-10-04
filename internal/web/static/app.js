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

  // A rename preview on page load when a folder is preselected.
  document.addEventListener("DOMContentLoaded", function () {
    var form = document.querySelector("form.rename");
    if (form && form.querySelector("input[name=clip]:checked")) htmx.trigger(form, "submit");
  });
})();
