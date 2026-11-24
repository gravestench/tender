const assert = require("node:assert/strict");
const { chromium } = require(process.env.TENDER_PLAYWRIGHT || "playwright");
(async () => {
  const browser = await chromium.launch({
    executablePath: process.env.TENDER_CHROMIUM || undefined,
    args: ["--disable-background-networking", "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1", "--no-proxy-server"],
  });
  try {
    const mobile = process.env.TENDER_VIEW === "RedirectShopper";
    const viewport = mobile ? {width:390,height:844} : {width:1200,height:900};
    const ctx = await browser.newContext({viewport, isMobile:mobile, serviceWorkers:"block"});
    const denied = [];
    await ctx.route("**/*", route => {
      if (new URL(route.request().url()).origin === process.env.TENDER_ORIGIN) return route.continue();
      denied.push(route.request().resourceType());
      return route.abort();
    });
    await ctx.routeWebSocket("**/*", route => route.close());
    let pages = 0;
    ctx.on("page", () => { pages++; });
    const page = await ctx.newPage();
    await page.goto(process.env.TENDER_TICKET);
    const order = page.url();
    await page.getByRole("button", {name: "Prepare card checkout"}).click();
    await page.getByRole("button", {name:"Use synthetic card"}).waitFor();
    assert.equal(await page.locator('input:not([type="hidden"]),iframe').count(), 0);
    await page.reload();
    await page.getByRole("button", {name:"Use synthetic card"}).click();
    await page.getByText("This payment needs review.", {exact:false}).waitFor();
    assert.equal(page.url(), order);
    assert.equal(pages, 1);
    assert.deepEqual(denied, []);
  } finally { await browser.close(); }
})().catch(() => {console.error("Offline card checkout browser assertions failed"); process.exitCode = 1;});
