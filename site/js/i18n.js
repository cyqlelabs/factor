// Applies the visitor's dictionary to the page and offers t() for strings
// that only exist in script. English is the page itself, so a key missing
// from a dictionary falls back to whatever the HTML already says.
(function () {
  var EN = {
    "dl.linux": "Download for Linux",
    "dl.mac": "Download for macOS",
    "dl.win": "Download for Windows",
    "dl.note.unix": "Then run <code>chmod +x {file} && ./{file} init</code>",
    "dl.note.win": "Installs for your account. No administrator needed.",
    "dl.note.any": "Builds for Linux, macOS and Windows.",
    "copy.done": "Copied",
    "proof.clean": "This page made <b>{n}</b> requests, all of them to <b>{host}</b>. Not one went anywhere else.",
    "proof.mixed": "This page made <b>{n}</b> requests; <b>{x}</b> of them went somewhere other than {host}.",
    "proof.local": "this computer",
    "hands.say1": "“Export is in D4.”",
    "hands.say2": "“Closer: it's in B3.”",
    "hands.say3": "“Clicked. Exporting now.”"
  };
  var dict = null;

  window.__t = function (key, vars) {
    var s = (dict && dict[key]) || EN[key] || key;
    if (vars) s = s.replace(/\{(\w+)\}/g, function (m, k) { return k in vars ? vars[k] : m; });
    return s;
  };

  function apply(d) {
    document.querySelectorAll("[data-i18n]").forEach(function (el) {
      var v = d[el.getAttribute("data-i18n")];
      if (v != null) el.textContent = v;
    });
    document.querySelectorAll("[data-i18n-html]").forEach(function (el) {
      var v = d[el.getAttribute("data-i18n-html")];
      if (v != null) el.innerHTML = v;
    });
    document.querySelectorAll("[data-i18n-attr]").forEach(function (el) {
      el.getAttribute("data-i18n-attr").split(";").forEach(function (pair) {
        var p = pair.split(":"), v = d[p[1]];
        if (v != null) el.setAttribute(p[0], v);
      });
    });
  }

  window.__i18n = (window.__i18n || Promise.resolve(null)).then(function (d) {
    dict = d;
    if (d) apply(d);
    document.documentElement.classList.remove("i18n-wait");
    return d;
  });
})();
