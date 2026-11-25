const assert = require("node:assert/strict");
const { chromium } = require(process.env.TENDER_PLAYWRIGHT || "playwright");

(async () => {
  const origin = process.env.TENDER_ORIGIN;
  const browser = await require("../../examples/consumer/browser-guard.cjs").launchBrowser(chromium, [origin]);
  try {
    const ctx = await browser.newContext({ serviceWorkers: "block" });
    const external = [];
    const opened = [];
    ctx.on("page", page => opened.push(page));
    await ctx.route("**/*", route => {
      if (new URL(route.request().url()).origin === origin) return route.continue();
      external.push(route.request().url());
      return route.abort();
    });
    await ctx.routeWebSocket("**/*", route => route.close());
    const page = await ctx.newPage();
    opened.length = 0;
    const waitStatus = async value => {
      await page.waitForFunction(expected => {
        const status = document.querySelector('[role="status"][aria-label="Payment status"]');
        return status && status.textContent.trim() === expected;
      }, value, { timeout: 3000 });
      const status = page.getByRole("status", { name: "Payment status" });
      assert.equal((await status.innerText()).trim(), value);
    };
    if (process.env.TENDER_VIEW === "wallet") {
      await page.route("**/wallet/start", route => route.abort());
      await page.goto(process.env.TENDER_GUEST);
      const checkout = page.url();
      await page.keyboard.press("Tab");
      assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), "Start synthetic wallet checkout");
      await page.keyboard.press("Enter");
      await waitStatus("PayPal is unavailable for this checkout.");
      assert.equal(await page.evaluate(() => document.activeElement?.id), "message", "error receives keyboard focus");
      assert.equal(await page.getByRole("button").count(), 0, "error hides unavailable controls");
      await page.reload();
      await page.getByRole("button", { name: "Start synthetic wallet checkout" }).waitFor();
      await page.keyboard.press("Tab");
      assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), "Start synthetic wallet checkout", "refresh restores reachable checkout");
      assert.equal(page.url(), checkout);
      assert.equal(opened.length, 0);
      assert.deepEqual(external, []);
      return;
    }
    if (process.env.TENDER_VIEW === "wallet_lifecycle") {
      await page.goto(process.env.TENDER_GUEST);
      const checkout = page.url();
      await page.keyboard.press("Tab");
      assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), "Start synthetic wallet checkout");
      await page.keyboard.press("Enter");
      await waitStatus("Approve or cancel the synthetic payment.");
      assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), "Approve synthetic payment", "approval receives focus after start control hides");
      await page.keyboard.press("Tab");
      assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), "Cancel approval");
      await page.keyboard.press("Enter");
      await waitStatus("Approval cancelled. No payment collected.");
      assert.equal(await page.evaluate(() => document.activeElement?.id), "message", "cancel result receives focus");
      await page.keyboard.press("Tab");
      assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), "Start synthetic wallet checkout", "cancel leaves a reachable retry");
      await page.keyboard.press("Enter");
      await waitStatus("Approve or cancel the synthetic payment.");
      assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), "Approve synthetic payment");
      await page.keyboard.press("Enter");
      await waitStatus("Payment captured.");
      assert.equal(await page.evaluate(() => document.activeElement?.id), "message", "capture result receives focus");
      assert.equal(await page.getByRole("button").count(), 0, "paid checkout has no active controls");
      assert.equal(page.url(), checkout);
      assert.equal(opened.length, 0);
      assert.deepEqual(external, []);
      return;
    }
    await page.goto(process.env.TENDER_GUEST);
    const checkout = page.url();
    const focused = () => page.evaluate(() => document.activeElement?.id || document.activeElement?.tagName.toLowerCase());
    await page.keyboard.press("Tab");
    assert.equal(await focused(), "scenario", "scenario selector is first in keyboard order");
    assert.equal(await page.getByRole("combobox", { name: "Scenario" }).count(), 1);
    await page.keyboard.press("Tab");
    assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), "Run simulation");
    const submit = page.waitForResponse(response => response.request().method() === "POST");
    await page.keyboard.press("Enter");
    assert.equal((await submit).status(), 303);
    await waitStatus("pending");
    assert.equal(await focused(), "payment-status", "pending result receives keyboard focus");
    assert.equal(page.url(), checkout, "checkout remains on its own page");
    await page.reload();
    await waitStatus("pending");
    assert.equal(await focused(), "payment-status", "refresh keeps the result discoverable");
    await page.keyboard.press("Tab");
    assert.notEqual(await focused(), "payment-status", "result does not trap keyboard focus");

    await page.goto(process.env.TENDER_ACCOUNT);
    const accountCheckout = page.url();
    await page.keyboard.press("Tab");
    assert.equal(await focused(), "scenario");
    await page.getByRole("combobox", { name: "Scenario" }).selectOption("action");
    await page.keyboard.press("Tab");
    const actionSubmit = page.waitForResponse(response => response.request().method() === "POST");
    await page.keyboard.press("Enter");
    assert.equal((await actionSubmit).status(), 303);
    await waitStatus("requires_action");
    assert.equal(await focused(), "payment-status");
    await page.keyboard.press("Tab");
    assert.equal(await page.evaluate(() => document.activeElement?.textContent.trim()), "Complete synthetic action");
    const confirm = page.waitForResponse(response => response.request().method() === "POST");
    await page.keyboard.press("Enter");
    assert.equal((await confirm).status(), 303);
    await waitStatus("captured");
    assert.equal(await focused(), "payment-status");
    assert.equal(page.url(), accountCheckout);
    assert.equal(opened.length, 0, "checkout opens no additional window");
    assert.deepEqual(external, [], "checkout makes no external request");
  } finally {
    await browser.close();
  }
})().catch(err => { console.error(err); process.exitCode = 1; });
