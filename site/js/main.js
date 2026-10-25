// Factor homepage behaviour: the coordinate sheet, the download button, the
// install tabs, and the scroll choreography. Everything that moves is gated on
// prefers-reduced-motion; everything that informs works without GSAP.
(function () {
  "use strict";

  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };
  var root = document.documentElement;
  var t = function (k, v) { return window.__t ? window.__t(k, v) : k; };
  var LETTERS = "ABCDEFGHIJKL";
  var REL = "https://github.com/cyqlelabs/factor/releases/latest/download/";

  (window.__i18n || Promise.resolve()).then(function () {
    return document.fonts && document.fonts.ready;
  }).then(start, start);

  var started = false;
  function start() {
    if (started) return;
    started = true;
    var grid = sheet();
    download();
    tabs();
    copyButtons();
    language();
    proof();
    nav();
    motion(grid);
  }

  /* ---- the coordinate sheet ------------------------------------------------ */

  function sheet() {
    var g = { cell: 0, gut: 0, cols: 12, rx: 0, desk: false };
    var rx = $(".ruler-x"), ry = $(".ruler-y"), readout = $(".readout b"), cellEl = $(".sheet-cell");
    var pool = [], px = -1, py = -1;
    var fine = matchMedia("(pointer: fine)");

    function measure() {
      var w = root.clientWidth;
      g.desk = w >= 1024;
      g.gut = g.desk ? 28 : 0;
      g.rx = g.desk ? 22 : 0;
      g.cols = w < 640 ? 6 : 12;
      g.cell = (w - g.gut) / g.cols;
      root.style.setProperty("--cell", g.cell + "px");
      rx.textContent = "";
      for (var c = 0; c < g.cols; c++) {
        var s = document.createElement("span");
        s.textContent = LETTERS[c];
        s.style.left = c * g.cell + "px";
        rx.appendChild(s);
      }
      var need = Math.ceil(innerHeight / g.cell) + 2;
      while (pool.length < need) { var r = document.createElement("span"); ry.appendChild(r); pool.push(r); }
      rows();
      point();
    }

    function rows() {
      if (!g.desk) return;
      var y = scrollY, first = Math.floor(y / g.cell);
      pool.forEach(function (s, i) {
        var n = first + i;
        s.textContent = n + 1;
        s.style.transform = "translateY(" + (n * g.cell - y) + "px)";
        s.dataset.row = n;
      });
    }

    function point() {
      if (px < 0 || !g.desk) return;
      var col = Math.max(0, Math.min(g.cols - 1, Math.floor((px - g.gut) / g.cell)));
      var row = Math.max(0, Math.floor((py + scrollY) / g.cell));
      cellEl.style.transform = "translate(" + (g.gut + col * g.cell) + "px," + row * g.cell + "px)";
      readout.textContent = LETTERS[col] + (row + 1);
      $$("span", rx).forEach(function (s, i) { s.classList.toggle("on", i === col); });
      pool.forEach(function (s) { s.classList.toggle("on", +s.dataset.row === row); });
    }

    g.cellAt = function (x, docY) {
      var col = Math.max(0, Math.min(g.cols - 1, Math.floor((x - g.gut) / g.cell)));
      return LETTERS[col] + (Math.floor(docY / g.cell) + 1);
    };

    if (fine.matches) {
      addEventListener("pointermove", function (e) {
        if (e.pointerType !== "mouse") return;
        px = e.clientX; py = e.clientY;
        if (!root.classList.contains("has-pointer")) root.classList.add("has-pointer");
        cellEl.classList.add("on");
        point();
      }, { passive: true });
      document.addEventListener("pointerleave", function () { cellEl.classList.remove("on"); });
      addEventListener("pointerdown", function (e) {
        if (e.pointerType !== "mouse") return;
        cellEl.classList.add("hit");
        setTimeout(function () { cellEl.classList.remove("hit"); }, 240);
      });
    }
    addEventListener("scroll", function () { rows(); point(); }, { passive: true });
    addEventListener("resize", measure);
    measure();
    return g;
  }

  // Each section label names the cell its heading starts in — the same
  // coordinate the readout shows when the pointer rests on it.
  function cellRefs(grid, pins) {
    $$("[data-cellref]").forEach(function (el) {
      var sec = el.closest("section"), h = $("h2", sec);
      if (!h) return;
      var r = h.getBoundingClientRect(), y = r.top + scrollY;
      var pin = pins.get(sec);
      if (pin && pin.isActive) y = pin.start + (r.top - pin.pin.getBoundingClientRect().top);
      el.textContent = grid.cellAt(r.left + 2, y + 2);
    });
  }

  /* ---- download, tabs, copy, language, proof -------------------------------- */

  function platform() {
    var p = ((navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || "").toLowerCase();
    var ua = navigator.userAgent.toLowerCase();
    if (/android|iphone|ipad|ipod/.test(ua) || (p.indexOf("mac") >= 0 && navigator.maxTouchPoints > 1)) return "other";
    if (p.indexOf("win") >= 0) return "win";
    if (p.indexOf("mac") >= 0) return "mac";
    if (p.indexOf("linux") >= 0 || p.indexOf("x11") >= 0) return "linux";
    return "other";
  }

  function download() {
    var os = platform();
    var file = { linux: "factor-linux-amd64", mac: "factor-darwin-arm64", win: "factor-windows-amd64.msi" }[os];
    $$("[data-dl]").forEach(function (a) {
      if (!file) return;
      a.href = REL + file;
      $("[data-dl-label]", a).textContent = t("dl." + os);
    });
    var note = $("[data-dl-note]");
    if (note) note.innerHTML = os === "win" ? t("dl.note.win") : file ? t("dl.note.unix", { file: file }) : t("dl.note.any");
    var tab = { linux: "tab-linux", mac: "tab-mac", win: "tab-win" }[os];
    if (tab) select($("#" + tab));
  }

  function select(tab) {
    var list = tab.closest("[role=tablist]");
    $$("[role=tab]", list).forEach(function (b) {
      var on = b === tab;
      b.setAttribute("aria-selected", on);
      b.tabIndex = on ? 0 : -1;
      $("#" + b.getAttribute("aria-controls")).hidden = !on;
    });
  }

  function tabs() {
    var list = $(".tabs");
    if (!list) return;
    var all = $$("[role=tab]", list);
    all.forEach(function (b, i) {
      b.addEventListener("click", function () { select(b); });
      b.addEventListener("keydown", function (e) {
        var d = { ArrowRight: 1, ArrowLeft: -1 }[e.key];
        if (e.key === "Home") d = -i;
        if (e.key === "End") d = all.length - 1 - i;
        if (d === undefined) return;
        e.preventDefault();
        var next = all[(i + d + all.length) % all.length];
        select(next);
        next.focus();
      });
    });
  }

  function copyButtons() {
    $$(".copy").forEach(function (b) {
      b.addEventListener("click", function () {
        var text = $("code", b.parentNode).textContent;
        var done = function () {
          b.classList.add("ok");
          b.innerHTML = '<svg aria-hidden="true"><use href="#i-check"/></svg><span class="sr-only">' + t("copy.done") + "</span>";
          setTimeout(function () {
            b.classList.remove("ok");
            b.innerHTML = '<svg aria-hidden="true"><use href="#i-copy"/></svg>';
          }, 1600);
        };
        if (navigator.clipboard) navigator.clipboard.writeText(text).then(done, function () {});
      });
    });
  }

  function language() {
    var sel = $("#lang");
    if (!sel) return;
    sel.value = root.getAttribute("data-lang") || "en";
    sel.addEventListener("change", function () {
      try { localStorage.setItem("factor-lang", sel.value); } catch (e) {}
      var u = new URL(location.href);
      u.searchParams.set("lang", sel.value);
      location.href = u.toString();
    });
  }

  // A no-telemetry product should be able to prove its own page: count what
  // this visit actually fetched, and from where.
  function proof() {
    var el = $("[data-proof]");
    if (!el || !window.performance || !performance.getEntriesByType) return;
    var run = function () {
      var host = location.host;
      var entries = performance.getEntriesByType("resource").concat(performance.getEntriesByType("navigation"));
      var urls = entries.map(function (e) { return e.name; }).filter(function (u) { return /^https?:|^file:/.test(u); });
      var away = urls.filter(function (u) { try { return new URL(u).host !== host; } catch (e) { return false; } });
      var label = host || t("proof.local");
      el.innerHTML = away.length
        ? t("proof.mixed", { n: urls.length, x: away.length, host: label })
        : t("proof.clean", { n: urls.length, host: label });
    };
    if (document.readyState === "complete") setTimeout(run, 400);
    else addEventListener("load", function () { setTimeout(run, 400); });
  }

  function nav() {
    var n = $(".nav");
    var on = function () { n.classList.toggle("solid", scrollY > 8); };
    addEventListener("scroll", on, { passive: true });
    on();
  }

  /* ---- motion ------------------------------------------------------------- */

  function fitHero() {
    var h1 = $(".hero-title");
    if (!h1) return;
    h1.style.fontSize = "";
    if (root.clientWidth < 640) return;
    var prev = h1.style.getPropertyValue("--w");
    h1.style.setProperty("--w", "125%");
    var cs = parseFloat(getComputedStyle(h1).fontSize);
    var widest = Math.max.apply(null, $$(".line > span", h1).map(function (s) { return s.getBoundingClientRect().width; }));
    var size = Math.min(cs * (h1.clientWidth / widest) * 0.995, 140);
    h1.style.fontSize = size + "px";
    if (prev) h1.style.setProperty("--w", prev); else h1.style.removeProperty("--w");
  }

  function bars(el, n, seed) {
    var html = "";
    for (var i = 0; i < n; i++) {
      seed = (seed * 9301 + 49297) % 233280;
      var r = seed / 233280, env = Math.sin((i / n) * Math.PI);
      html += '<i style="--h:' + (0.12 + env * (0.3 + r * 0.6)).toFixed(3) + ";--d:-" + (r * 0.9).toFixed(2) + 's"></i>';
    }
    el.innerHTML = html;
  }

  function screenLabels() {
    var lab = $(".vgrid-labels"), sub = $(".subgrid");
    if (!lab) return;
    var html = "";
    for (var c = 0; c < 8; c++) html += '<span style="left:' + c * 12.5 + '%;top:0">' + LETTERS[c] + "</span>";
    for (var r = 1; r < 5; r++) html += '<span style="left:0;top:' + r * 20 + '%">' + (r + 1) + "</span>";
    lab.innerHTML = html;
    html = '<i class="tgt"></i>';
    for (c = 0; c < 4; c++) html += '<span style="left:' + c * 25 + '%;top:0">' + LETTERS[c] + "</span>";
    for (r = 1; r < 4; r++) html += '<span style="left:0;top:' + r * 25 + '%">' + (r + 1) + "</span>";
    sub.innerHTML = html;
  }

  // Lines from each element's edge to a target's center, drawn in the
  // container's own pixel space so dots travelling them stay round.
  function wire(svg, from, to) {
    var w = svg.parentNode.clientWidth, h = svg.parentNode.clientHeight;
    svg.setAttribute("viewBox", "0 0 " + w + " " + h);
    var tx = to.offsetLeft + to.offsetWidth / 2, ty = to.offsetTop + to.offsetHeight / 2;
    $$("path", svg).forEach(function (p, i) {
      var b = from[i], left = b.offsetLeft + b.offsetWidth / 2 < tx;
      var x = left ? b.offsetLeft + b.offsetWidth : b.offsetLeft, y = b.offsetTop + b.offsetHeight / 2;
      var mx = (x + tx) / 2;
      p.setAttribute("d", "M" + x + " " + y + " C " + mx + " " + y + ", " + mx + " " + ty + ", " + tx + " " + ty);
    });
  }

  function motion(grid) {
    screenLabels();
    bars($(".voice-you .bars"), 56, 7);
    bars($(".voice-guest .bars"), 56, 23);
    fitHero();

    var reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
    var pins = new Map();
    var lift = function () { root.classList.remove("pending"); };

    if (!window.gsap || !window.ScrollTrigger) {
      lift();
      addEventListener("resize", fitHero);
      cellRefs(grid, pins);
      return;
    }
    var gsap = window.gsap, ST = window.ScrollTrigger;
    gsap.registerPlugin(ST);
    if (window.SplitText) gsap.registerPlugin(window.SplitText);
    if (window.ScrambleTextPlugin) gsap.registerPlugin(window.ScrambleTextPlugin);
    if (window.DrawSVGPlugin) gsap.registerPlugin(window.DrawSVGPlugin);
    if (window.MotionPathPlugin) gsap.registerPlugin(window.MotionPathPlugin);

    var lenis = null;
    if (!reduce && window.Lenis) {
      lenis = new window.Lenis({ lerp: 0.11 });
      lenis.on("scroll", ST.update);
      gsap.ticker.add(function (time) { lenis.raf(time * 1000); });
      gsap.ticker.lagSmoothing(0);
    }
    anchors(lenis, grid);

    ST.addEventListener("refreshInit", fitHero);
    ST.addEventListener("refresh", function () { cellRefs(grid, pins); });

    lift();
    if (reduce) {
      // The end state of every scene, drawn once and kept in step with resizes.
      var still = function () {
        if (root.clientWidth >= 900) wire($(".reach-lines"), $$(".door"), $(".reach-core"));
        memLines();
      };
      still();
      addEventListener("resize", still);
      $$(".m-drop").forEach(function (m) { m.style.opacity = 0.3; });
      $(".room").classList.add("is-shared");
      $(".st-private").style.display = "none";
      $(".st-shared").style.opacity = 1;
      $(".space-main").style.opacity = 0.3;
      $(".app-export").classList.add("done");
      ST.refresh();
      return;
    }

    hero(gsap);
    var mm = gsap.matchMedia();
    // GSAP runs the callback only while some condition matches, so "all" keeps
    // it alive on a narrow touch screen where the others are all false.
    mm.add({ all: "all", wide: "(min-width: 900px)", mid: "(min-width: 640px)", fine: "(pointer: fine)" }, function (ctx) {
      var c = ctx.conditions, hooks = [], undoPointer = null;
      var onRefresh = function () { hooks.forEach(function (f) { f(); }); };
      ST.addEventListener("refreshInit", onRefresh);
      heroScroll(gsap, c);
      if (c.wide && c.fine) undoPointer = pointerDepth(gsap);
      reach(gsap, c, pins, hooks);
      hands(gsap, c, pins);
      work(gsap, c);
      memory(gsap, c, pins, hooks);
      room(gsap, c, pins);
      honest(gsap, c);
      install(gsap);
      parallax(gsap);
      return function () {
        pins.clear();
        ST.removeEventListener("refreshInit", onRefresh);
        if (undoPointer) undoPointer();
        $$(".packet").forEach(function (p) { p.remove(); });
        $$("[data-depth]").forEach(function (l) { l.style.translate = ""; });
      };
    });
  }

  function anchors(lenis, grid) {
    $$('a[href^="#"]').forEach(function (a) {
      a.addEventListener("click", function (e) {
        var id = a.getAttribute("href");
        var target = id === "#top" ? document.body : $(id);
        if (!target) return;
        e.preventDefault();
        var offset = -(grid.rx + 60 + 8);
        if (lenis) lenis.scrollTo(id === "#top" ? 0 : target, { offset: offset, duration: 1.4 });
        else if (id === "#top") scrollTo(0, 0);
        else scrollTo(0, target.getBoundingClientRect().top + scrollY + offset);
        if (id !== "#top") {
          if (!target.hasAttribute("tabindex")) target.setAttribute("tabindex", "-1");
          target.focus({ preventScroll: true });
        }
        history.replaceState(null, "", id === "#top" ? location.pathname + location.search : id);
      });
    });
  }

  /* hero: an orchestrated arrival, then layers that part as you leave */
  function hero(gsap) {
    var tl = gsap.timeline({ defaults: { ease: "expo.out", duration: 1.1 } });
    tl.from(".hero .eyebrow", { y: 14, autoAlpha: 0, duration: 0.8 }, 0.1)
      .from(".hero-title .line > span", { yPercent: 108, duration: 1.25, stagger: 0.11 }, 0.15)
      .from(".hero-lead", { y: 24, autoAlpha: 0 }, 0.55)
      .from(".hero-cta, .cta-note", { y: 18, autoAlpha: 0, stagger: 0.08 }, 0.65)
      .from(".plane-grid, .plane-hit", { autoAlpha: 0, duration: 1.4 }, 0.25)
      .from(".plane-l, .plane-n", { autoAlpha: 0, stagger: 0.035, duration: 0.4 }, 0.75)
      .from(".stage-mark > svg", { scale: 0.35, autoAlpha: 0, duration: 1.3, ease: "back.out(1.5)" }, 0.55)
      .from(".frag-body", { y: 46, autoAlpha: 0, duration: 1.1, stagger: 0.13 }, 0.85)
      .from(".hero-letters span", { yPercent: 30, autoAlpha: 0, duration: 1.8, stagger: 0.12 }, 0);
  }

  function heroScroll(gsap, c) {
    var st = { trigger: ".hero", start: "top top", end: "bottom top", scrub: true };
    gsap.fromTo(".hero-title", { "--w": c.mid ? "125%" : "112%" }, { "--w": c.mid ? "80%" : "100%", yPercent: -26, ease: "none", scrollTrigger: st });
    gsap.to(".hero-rest", { y: -60, autoAlpha: 0.15, ease: "none", scrollTrigger: st });
    gsap.to(".hero-letters", { yPercent: 40, ease: "none", scrollTrigger: st });
    var depth = { ".stage-plane": 60, ".stage-mark": -40, ".frag-term": -130, ".frag-call": -220, ".frag-tg": -90, ".frag-mic": -280 };
    Object.keys(depth).forEach(function (sel) {
      gsap.to(sel, { y: c.wide ? depth[sel] : depth[sel] * 0.45, ease: "none", scrollTrigger: st });
    });
  }

  // Layers drift against the pointer by their depth; uses the independent
  // `translate` property so it composes with the scroll transforms.
  function pointerDepth(gsap) {
    var stage = $(".stage"), layers = $$("[data-depth]", stage);
    var tx = 0, ty = 0, x = 0, y = 0;
    var onMove = function (e) {
      tx = (e.clientX / innerWidth - 0.5) * 2;
      ty = (e.clientY / innerHeight - 0.5) * 2;
    };
    addEventListener("pointermove", onMove, { passive: true });
    var tick = function () {
      x += (tx - x) * 0.06; y += (ty - y) * 0.06;
      if (scrollY > innerHeight * 1.2) return;
      layers.forEach(function (l) {
        var d = +l.dataset.depth;
        l.style.translate = (-x * d * 14).toFixed(2) + "px " + (-y * d * 10).toFixed(2) + "px";
      });
    };
    gsap.ticker.add(tick);
    return function () { removeEventListener("pointermove", onMove); gsap.ticker.remove(tick); };
  }

  /* reach: four doors converge on one agent */
  function reach(gsap, c, pins, hooks) {
    if (!c.wide) {
      $$(".door").forEach(function (d) {
        gsap.from(d, { y: 40, autoAlpha: 0, duration: 0.9, ease: "expo.out", scrollTrigger: { trigger: d, start: "top 88%" } });
      });
      return;
    }
    var svg = $(".reach-lines"), doors = $$(".door"), core = $(".reach-core");
    var draw = function () { wire(svg, doors, core); };
    draw();
    hooks.push(draw);
    var tl = gsap.timeline({
      defaults: { ease: "power2.out" },
      scrollTrigger: { trigger: ".reach", start: "top top", end: "+=130%", pin: true, scrub: 0.8, invalidateOnRefresh: true }
    });
    pins.set($(".reach"), tl.scrollTrigger);
    var from = [[-1, -1], [1, -1], [-1, 1], [1, 1]];
    doors.forEach(function (d, i) {
      tl.from(d, {
        x: function () { return from[i][0] * innerWidth * 0.22; },
        y: function () { return from[i][1] * innerHeight * 0.18; },
        autoAlpha: 0, scale: 0.9, duration: 1
      }, i * 0.18);
    });
    tl.from(core, { scale: 0.4, autoAlpha: 0, duration: 0.8, ease: "back.out(1.6)" }, 0.5)
      .from($$("path", svg), { drawSVG: "0%", duration: 0.9, stagger: 0.12 }, 0.9)
      .to(core, { scale: 1.12, duration: 0.25, yoyo: true, repeat: 1, ease: "sine.inOut" }, 1.9)
      .fromTo(packets(gsap, svg), { autoAlpha: 0 }, { autoAlpha: 1, duration: 0.3 }, 1.9)
      .to({}, { duration: 0.6 });
  }

  function packets(gsap, svg) {
    var ns = "http://www.w3.org/2000/svg", dots = [];
    $$("path", svg).forEach(function (p, i) {
      var dot = document.createElementNS(ns, "circle");
      dot.setAttribute("r", 4);
      dot.setAttribute("class", "packet");
      dot.setAttribute("fill", i % 2 ? "#f2c75c" : "#0369a1");
      svg.appendChild(dot);
      dots.push(dot);
      gsap.to(dot, {
        motionPath: { path: p, align: p, alignOrigin: [0.5, 0.5] },
        duration: 2.4, repeat: -1, delay: i * 0.6, ease: "power1.inOut",
        scrollTrigger: { trigger: ".reach", start: "top 20%", toggleActions: "play pause resume pause" }
      });
    });
    return dots;
  }

  /* hands: screen_view → screen_zoom → mouse click, pinned and scrubbed */
  function hands(gsap, c, pins) {
    var steps = $$(".step"), say = $(".say"), tool = $(".screen-tool span"), btn = $(".app-export");
    var state = -1;
    var tools = ["screen_view", "screen_zoom cell=D4", "mouse click cell=B3"];
    var set = function (n) {
      if (n === state) return;
      state = n;
      steps.forEach(function (s, i) { s.classList.toggle("is-on", i === n); });
      say.textContent = t("hands.say" + (n + 1));
      tool.textContent = tools[n];
    };
    var tl = gsap.timeline({
      defaults: { ease: "power2.inOut" },
      scrollTrigger: {
        trigger: ".hands-pin", start: "top top", end: c.wide ? "+=240%" : "+=200%", pin: true, scrub: 0.7,
        onUpdate: function (self) {
          var p = self.progress;
          set(p < 0.3 ? 0 : p < 0.66 ? 1 : 2);
          btn.classList.toggle("done", p > 0.82);
        }
      }
    });
    pins.set($(".hands"), tl.scrollTrigger);
    tl.from(".vgrid", { autoAlpha: 0, duration: 0.8 }, 0)
      .from(".vgrid-labels span", { autoAlpha: 0, scale: 0.6, stagger: 0.05, duration: 0.4 }, 0.2)
      .from(".vcell", { autoAlpha: 0, scale: 1.6, duration: 0.7, ease: "back.out(1.4)" }, 1.1)
      .to(".screen-canvas", { scale: 5, xPercent: -168.75, yPercent: -300, duration: 2.4 }, 2.6)
      .to(".vgrid-labels, .vcell", { autoAlpha: 0, duration: 0.6 }, 2.8)
      .to(".subgrid", { autoAlpha: 1, duration: 0.8 }, 4.2)
      .from(".subgrid .tgt", { autoAlpha: 0, duration: 0.5 }, 5.1)
      .to(".cursor", { left: "42.2%", top: "62.5%", duration: 1.6, ease: "power3.inOut" }, 5.6)
      .fromTo(".click", { scale: 0.2, autoAlpha: 1 }, { scale: 1.8, autoAlpha: 0, duration: 0.7, ease: "power2.out" }, 7.3)
      .to(".cursor", { scale: 0.85, duration: 0.15, yoyo: true, repeat: 1 }, 7.25)
      .to({}, { duration: 1.2 });
    set(0);
  }

  /* work: the conversation assembles itself as you read */
  function work(gsap, c) {
    $$(".thread > *").forEach(function (m) {
      var fromRight = m.classList.contains("msg-you") || m.classList.contains("notify");
      gsap.from(m, {
        x: fromRight ? 40 : -40, y: 30, autoAlpha: 0, ease: "power3.out",
        scrollTrigger: { trigger: m, start: "top 92%", end: "top 62%", scrub: 0.6 }
      });
    });
    gsap.fromTo(".lane", { x: c.wide ? 120 : 60 }, { x: c.wide ? -140 : -260, ease: "none", scrollTrigger: { trigger: ".lane", start: "top bottom", end: "bottom top", scrub: true } });
    gsap.from(".msg-steer", { rotate: -2.5, scale: 1.06, ease: "back.out(2)", scrollTrigger: { trigger: ".msg-steer", start: "top 75%", end: "top 55%", scrub: true } });
  }

  // The constellation joins the chips' resting centres; offsetLeft/Top
  // ignore the transforms the scroll timeline is applying at the time.
  function memLines() {
    var field = $(".mem-field"), svg = $(".mem-lines"), keep = $$(".m-keep");
    svg.setAttribute("viewBox", "0 0 " + field.clientWidth + " " + field.clientHeight);
    var p = keep.map(function (el) { return [el.offsetLeft, el.offsetTop]; });
    var d = [0, 1, 2, 3, 4, 5, 0].map(function (k, i) { return (i ? "L" : "M") + p[k][0] + " " + p[k][1]; }).join(" ");
    $("path", svg).setAttribute("d", d + " M" + p[1][0] + " " + p[1][1] + " L" + p[4][0] + " " + p[4][1]);
  }

  /* memory: trivia decays, what matters consolidates into a shape */
  function memory(gsap, c, pins, hooks) {
    var keep = $$(".m-keep"), drop = $$(".m-drop"), path = $(".mem-lines path");
    memLines();
    hooks.push(memLines);
    var seed = 11, rnd = function () { seed = (seed * 9301 + 49297) % 233280; return seed / 233280 - 0.5; };
    var tl = gsap.timeline({
      defaults: { ease: "power2.inOut" },
      scrollTrigger: { trigger: ".memory-pin", start: "top top", end: c.wide ? "+=150%" : "+=120%", pin: true, scrub: 0.8 }
    });
    pins.set($(".memory"), tl.scrollTrigger);
    keep.forEach(function (k) { tl.from(k, { x: rnd() * 160, y: rnd() * 120, duration: 1.4 }, 0); });
    drop.forEach(function (k, i) {
      var x = +k.style.getPropertyValue("--x") - 50, y = +k.style.getPropertyValue("--y") - 50;
      tl.to(k, { x: x * 3, y: y * 2, autoAlpha: 0, filter: "blur(6px)", scale: 0.8, duration: 1.2 }, 0.5 + i * 0.08);
    });
    tl.from(path, { drawSVG: "0%", duration: 1.3 }, 1.4)
      .from(".m-rule", { scale: 0.86, duration: 0.6, ease: "back.out(2)" }, 2.4)
      .to({}, { duration: 0.6 });

    gsap.from(".skill-card", { y: 60, autoAlpha: 0, duration: 1, ease: "expo.out", scrollTrigger: { trigger: ".skill", start: "top 75%" } });
    if (window.ScrambleTextPlugin) {
      var lines = gsap.timeline({ scrollTrigger: { trigger: ".skill", start: "top 70%" } });
      $$(".sk-l").forEach(function (l, i) {
        lines.fromTo(l, { scrambleText: { text: " ", chars: "" } }, {
          duration: 0.5 + l.textContent.length * 0.012, ease: "none",
          scrambleText: { text: l.textContent, chars: "abcdef0123456789-_/", speed: 0.8 }
        }, i * 0.22);
      });
    }
  }

  /* room: a second voice turns the room shared */
  function room(gsap, c, pins) {
    var sec = $(".room"), you = $(".voice-you"), guest = $(".voice-guest");
    var tl = gsap.timeline({
      defaults: { ease: "power2.out" },
      scrollTrigger: {
        trigger: ".room-pin", start: "top top", end: c.wide ? "+=190%" : "+=160%", pin: true, scrub: 0.8,
        onUpdate: function (self) {
          var p = self.progress;
          you.classList.toggle("speaking", p < 0.3 || p > 0.78);
          guest.classList.toggle("speaking", p > 0.24 && p < 0.55);
          sec.classList.toggle("is-shared", p > 0.42);
        }
      }
    });
    pins.set(sec, tl.scrollTrigger);
    tl.from(".voice-guest", { x: 40, autoAlpha: 0, duration: 0.8 }, 0.8)
      .to(".st-private", { autoAlpha: 0, y: -8, duration: 0.3 }, 2)
      .fromTo(".st-shared", { autoAlpha: 0, y: 8 }, { autoAlpha: 1, y: 0, duration: 0.3 }, 2.1)
      .to(".room-status .dot", { backgroundColor: "#f2c75c", boxShadow: "0 0 0 4px rgba(242,199,92,.25)", duration: 0.3 }, 2)
      .from(".room-say", { y: 24, scale: 0.94, autoAlpha: 0, duration: 0.6, ease: "back.out(1.8)" }, 2.4)
      .to(".space-main", { autoAlpha: 0.22, y: 10, filter: "blur(1.5px)", duration: 0.8 }, 3)
      .to({}, { duration: 1.2 });
    sec.classList.remove("is-shared");
  }

  function honest(gsap, c) {
    $$(".row").forEach(function (r) {
      gsap.from(r, { y: 50, autoAlpha: 0, duration: 1, ease: "expo.out", scrollTrigger: { trigger: r, start: "top 88%" } });
    });
    $$(".fig").forEach(function (f) {
      var s = +f.dataset.speed || 0.9, k = c.wide ? 900 : 260;
      gsap.fromTo(f, { y: (1 - s) * k }, { y: -(1 - s) * k, ease: "none", scrollTrigger: { trigger: f.parentNode, start: "top bottom", end: "bottom top", scrub: true } });
    });
  }

  function install(gsap) {
    var tl = gsap.timeline({ scrollTrigger: { trigger: ".wizard", start: "top 75%" } });
    tl.from(".wizard", { y: 50, autoAlpha: 0, duration: 0.9, ease: "expo.out" })
      .from(".wz li", { autoAlpha: 0, x: -12, duration: 0.35, stagger: 0.32, ease: "power2.out" }, 0.4)
      .from(".wz .ok", { scale: 0, duration: 0.3, stagger: 0.32, ease: "back.out(3)" }, 0.55)
      .from(".wz-done", { autoAlpha: 0, duration: 0.4 }, "+=0.2");
    gsap.from(".final-mark", { scale: 0.5, autoAlpha: 0, duration: 1.2, ease: "back.out(1.6)", scrollTrigger: { trigger: ".final", start: "top 70%" } });
    if (window.SplitText) {
      var split = new window.SplitText(".final-line", { type: "chars,words", charsClass: "ch" });
      gsap.from(split.chars, { yPercent: 90, autoAlpha: 0, rotateX: -70, duration: 1, stagger: 0.03, ease: "expo.out", scrollTrigger: { trigger: ".final", start: "top 65%" } });
    }
  }

  // The big outline coordinates drift behind everything, and each dark
  // section opens out from a framed screen to full bleed as it arrives.
  function parallax(gsap) {
    $$(".hands, .room, .install").forEach(function (sec) {
      gsap.fromTo(sec, { clipPath: "inset(0% 4% 0% 4%)" }, {
        clipPath: "inset(0% 0% 0% 0%)", ease: "none",
        scrollTrigger: { trigger: sec, start: "top bottom", end: "top 15%", scrub: true }
      });
    });
    gsap.fromTo(".hands-letters", { yPercent: -10 }, { yPercent: 60, ease: "none", scrollTrigger: { trigger: ".hands", start: "top bottom", end: "bottom top", scrub: true } });
  }
})();
