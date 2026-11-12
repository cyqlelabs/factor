// Renders the social cards: og.html at 1200×630, once per language, into
// site/assets/og-<lang>.png. Run `make og` after changing the card or the
// dictionary keys it reads (hero.t1, hero.t2, og.tag). Needs Chrome; set
// CHROME to its path when it is not google-chrome on PATH.
import { execFileSync } from "node:child_process";
import { readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const site = path.dirname(here);
const chrome = process.env.CHROME || "google-chrome";
const langs = ["en", ...readdirSync(path.join(site, "i18n")).map((f) => path.basename(f, ".js"))];

for (const lang of langs) {
  const out = path.join(site, "assets", `og-${lang}.png`);
  execFileSync(chrome, [
    "--headless=new", "--hide-scrollbars", "--force-device-scale-factor=1",
    "--window-size=1200,630", "--allow-file-access-from-files", "--virtual-time-budget=5000",
    `--screenshot=${out}`, `${pathToFileURL(path.join(here, "og.html"))}?lang=${lang}`,
  ], { stdio: "ignore" });
  console.log(path.relative(process.cwd(), out));
}
