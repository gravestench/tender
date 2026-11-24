const assert = require("node:assert/strict");
const { chromium } = require(process.env.TENDER_PLAYWRIGHT || "playwright");
const { launchBrowser, guardCheckoutPage } = require("./browser-guard.cjs");

(async () => {
  const origin = process.env.TENDER_ORIGIN;
  const browser = await launchBrowser(chromium, [origin]);
  try {
    const ctx = await browser.newContext();
    const visited = [];
    const outside = [];
    const page = await ctx.newPage();
    ctx.on("page", () => visited.push("additional window"));
    await ctx.route("**/*", route => {
      if (new URL(route.request().url()).origin === origin) return route.continue();
      outside.push(route.request().url());
      return route.abort();
    });
    await ctx.routeWebSocket("**/*", route => route.close());
    await page.goto(process.env.TENDER_TICKET);
    const url = page.url();
    const denied = await guardCheckoutPage(page, () => false);
    page.on("framenavigated", frame => {
      if (frame === page.mainFrame()) visited.push(frame.url());
    });
    if (process.env.TENDER_VIEW === "component") {
      await page.getByRole("button", {name: "Start synthetic wallet checkout"}).click();
      await page.getByRole("button", {name: "Cancel approval"}).click();
      await page.getByText("Approval cancelled. No payment collected.").waitFor();
      const read = () => page.evaluate(async () => (await fetch(location.pathname + "/status")).json());
      let state = await read();
      assert.equal(state.Fulfilled, false);
      assert.equal(state.Funds.Captured, 0);
      await page.getByRole("button", {name: "Start synthetic wallet checkout"}).click();
      await page.getByRole("button", {name: "Approve synthetic payment"}).click();
      await page.getByText("Payment captured.").waitFor();
      state = await read();
      assert.equal(state.Status, "captured");
      assert.equal(state.Funds.Captured, state.Minor);
      assert.equal(state.Fulfilled, true);
      await page.waitForFunction(async () => {
        const state = await (await fetch(location.pathname + "/status")).json();
        return state.CaptureReceiptProcessed === true;
      }, undefined, {polling: 50, timeout: 12000});
    } else {
      await page.getByText("PayPal is unavailable for this checkout.").waitFor();
      assert.equal(await page.getByRole("button").count(), 0);
      const state = await page.evaluate(async () => (await fetch(location.pathname + "/status")).json());
      assert.equal(state.Status, "unpaid");
      assert.equal(state.Fulfilled, false);
    }
    assert.equal(page.url(), url);
    assert.equal(denied(), false);
    assert.equal(ctx.pages().length, 1);
    assert.deepEqual(visited, []);
    assert.deepEqual(outside, []);
    await ctx.close();
  } finally {
    await browser.close();
  }
})().catch(err => { console.error(err); process.exitCode = 1; });
