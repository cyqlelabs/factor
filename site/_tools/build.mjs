// Builds the homepage for GitHub Pages into _site/ (or the directory named
// as the first argument): the site as it stands, plus a copy of index.html
// per dictionary at /<lang>/ whose head is already in that language. Link
// previews read the head without running scripts, so a page translated in
// the browser shares in English; these copies are what a shared /es/ link is
// read from. Every substitution must match exactly once and every key must be
// in the dictionary, so a changed head fails the deploy instead of shipping a
// page that is half English.
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const here = path.dirname(fileURLToPath(import.meta.url));
const site = path.dirname(here);
const out = path.resolve(process.argv[2] || path.join(site, "..", "_site"));
const origin = "https://fctr.app";
const tags = { pt: "pt-BR", zh: "zh-Hans" };
const locales = { en: "en_US", es: "es_ES", pt: "pt_BR", fr: "fr_FR", de: "de_DE", ja: "ja_JP", zh: "zh_CN" };

function once(html, from, to) {
  const n = html.split(from).length - 1;
  if (n !== 1) throw new Error(`expected one ${JSON.stringify(from)}, found ${n}`);
  return html.replace(from, () => to);
}

function escape(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

// The card's URL carries a hash of the image, because link previews cache an
// image by its URL and a re-rendered card at the same address is never seen.
function card(lang) {
  const file = `assets/og-${lang}.png`;
  const sum = crypto.createHash("sha256").update(fs.readFileSync(path.join(site, file))).digest("hex");
  return `${origin}/${file}?v=${sum.slice(0, 10)}`;
}

function translate(head, dict, lang) {
  const keys = head.match(/data-i18n(-attr)?="/g).length;
  let done = 0;
  head = head.replace(/(<title data-i18n="([^"]+)">)[^<]*(<\/title>)/g, (m, open, key, close) => {
    if (dict[key] == null) throw new Error(`${lang}: no ${key}`);
    done++;
    return open + escape(dict[key]) + close;
  });
  head = head.replace(/(data-i18n-attr="content:([^"]+)" content=")[^"]*"/g, (m, start, key) => {
    if (dict[key] == null) throw new Error(`${lang}: no ${key}`);
    done++;
    return start + escape(dict[key]) + '"';
  });
  if (done !== keys) throw new Error(`${lang}: translated ${done} of ${keys} strings in the head`);
  return head;
}

const page = fs.readFileSync(path.join(site, "index.html"), "utf8");
const enCard = `content="${origin}/assets/og-en.png"`;

fs.rmSync(out, { recursive: true, force: true });
fs.cpSync(site, out, { recursive: true, filter: (src) => src !== here });
fs.writeFileSync(path.join(out, "index.html"), once(page, enCard, `content="${card("en")}"`));

for (const file of fs.readdirSync(path.join(site, "i18n"))) {
  const lang = path.basename(file, ".js");
  const sandbox = { window: {} };
  vm.runInNewContext(fs.readFileSync(path.join(site, "i18n", file), "utf8"), sandbox);
  const cut = page.indexOf("</head>");
  let html = translate(page.slice(0, cut), sandbox.window.FACTOR_DICT, lang) + page.slice(cut);
  html = once(html, '<html lang="en" data-lang="en">', `<html lang="${tags[lang] || lang}" data-lang="${lang}">`);
  html = once(html, `<link rel="canonical" href="${origin}/">`, `<link rel="canonical" href="${origin}/${lang}/">`);
  html = once(html, `<meta property="og:url" content="${origin}/">`, `<meta property="og:url" content="${origin}/${lang}/">`);
  html = once(html, `<meta property="og:locale" content="en_US">`, `<meta property="og:locale" content="${locales[lang]}">`);
  html = once(html, `<meta property="og:locale:alternate" content="${locales[lang]}">`, `<meta property="og:locale:alternate" content="en_US">`);
  html = once(html, enCard, `content="${card(lang)}"`);
  fs.mkdirSync(path.join(out, lang));
  fs.writeFileSync(path.join(out, lang, "index.html"), html);
}
console.log(path.relative(process.cwd(), out));
