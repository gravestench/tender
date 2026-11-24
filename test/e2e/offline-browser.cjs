const assert = require("node:assert/strict");
const { chromium } = require(process.env.TENDER_PLAYWRIGHT || "playwright");

(async () => {
  const browser = await chromium.launch({
    executablePath: process.env.TENDER_CHROMIUM || undefined,
    args: ["--disable-background-networking", "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1", "--no-proxy-server"],
  });
  try {
    const origin = process.env.TENDER_ORIGIN;
    const ctx = await browser.newContext({serviceWorkers: "block"});
    const external = [];
    await ctx.route("**/*", route => {
      if (new URL(route.request().url()).origin === origin) return route.continue();
      external.push(route.request().url());
      return route.abort();
    });
    await ctx.routeWebSocket("**/*", route => route.close());
    const page = await ctx.newPage();
    await page.goto(process.env.TENDER_GUEST);
    const checkout = page.url();
    await page.getByLabel("Scenario").waitFor();
    assert.equal(await page.locator('input:not([type="hidden"])').count(), 0);
    assert.equal(await page.locator("iframe").count(), 0);
    await page.getByLabel("Scenario").selectOption("collected");
    const submitted = page.waitForResponse(response => response.request().method() === "POST");
    await page.getByRole("button", {name: "Run simulation"}).click();
    const response = await submitted;
    if (response.status() !== 303) throw new Error("checkout HTTP " + response.status() + ": " + await response.text());
    await page.getByText("captured", {exact: true}).waitFor();
    assert.equal(page.url(), checkout);
    assert.equal(ctx.pages().length, 1);
    assert.deepEqual(external, []);
  } finally {
    await browser.close();
  }
})().catch(err => {console.error(err); process.exitCode = 1;});
