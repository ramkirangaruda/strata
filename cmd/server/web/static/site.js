(function () {
  "use strict";

  var reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  function splitReveal(root) {
    var i = 0;
    function walk(node) {
      if (node.nodeType === 3) {
        var parts = node.textContent.split(/(\s+)/);
        var frag = document.createDocumentFragment();
        parts.forEach(function (part) {
          if (part === "") return;
          if (/^\s+$/.test(part)) { frag.appendChild(document.createTextNode(part)); return; }
          var span = document.createElement("span");
          span.className = "rv-word";
          span.style.setProperty("--i", i++);
          span.textContent = part;
          frag.appendChild(span);
        });
        node.parentNode.replaceChild(frag, node);
      } else if (node.nodeType === 1) {
        Array.prototype.slice.call(node.childNodes).forEach(walk);
      }
    }
    Array.prototype.slice.call(root.childNodes).forEach(walk);
  }

  function initReveal(selector) {
    if (!reducedMotion) document.querySelectorAll(selector).forEach(splitReveal);
  }

  function initSpotlight(selector) {
    if (reducedMotion || !window.matchMedia("(hover: hover)").matches) return;
    document.querySelectorAll(selector).forEach(function (el) {
      el.addEventListener("mousemove", function (e) {
        var r = el.getBoundingClientRect();
        el.style.setProperty("--x", (e.clientX - r.left) + "px");
        el.style.setProperty("--y", (e.clientY - r.top) + "px");
      });
    });
  }

  function initScrollReveal(selector) {
    var els = document.querySelectorAll(selector || ".reveal");
    if (reducedMotion || !("IntersectionObserver" in window)) {
      els.forEach(function (e) { e.classList.add("in"); });
      return;
    }
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (en.isIntersecting) { en.target.classList.add("in"); io.unobserve(en.target); }
      });
    }, { threshold: 0.12 });
    els.forEach(function (e) { io.observe(e); });
  }

  function initHealthCheck() {
    var nav = document.getElementById("nav-status");
    if (!nav) return;
    fetch("/healthz").then(function (r) {
      nav.className = "status " + (r.ok ? "online" : "offline");
      nav.querySelector(".txt").textContent = r.ok ? "API online" : "API offline";
    }).catch(function () {
      nav.className = "status offline";
      nav.querySelector(".txt").textContent = "API offline";
    });
  }

  window.Site = {
    reducedMotion: reducedMotion,
    initReveal: initReveal,
    initSpotlight: initSpotlight,
    initScrollReveal: initScrollReveal,
    initHealthCheck: initHealthCheck
  };

  document.addEventListener("DOMContentLoaded", initHealthCheck);
})();
