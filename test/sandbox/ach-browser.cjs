// Explicit Adyen TEST-only probe. Go supplies a session through stdin. Only
// this browser driver knows the published synthetic giact routing/account pair.
const fs = require("node:fs");
const assert = require("node:assert/strict");
const { chromium } = require(process.env.TENDER_PLAYWRIGHT || "playwright");
const { launchBrowser, guardCheckoutPage } = require("../../examples/consumer/browser-guard.cjs");

async function run() {
  const session = JSON.parse(fs.readFileSync(0, "utf8"));
  const key = process.env.TENDER_ACH_CLIENT_KEY;
  const location = process.env.TENDER_ACH_ORIGIN + "/tender-ach-qualification";
  const shopper = "https://checkoutshopper-test.adyen.com";
  const cdn = "https://checkoutshopper-test.cdn.adyen.com";
  const sdk = cdn + "/checkoutshopper/sdk/6.45.2/adyen.js";
  const knownRouting = "122105278";
  const refused = process.env.TENDER_ACH_CASE === "refused";
  const knownAccount = refused ? "0000000005" : "0000000016";
  // The registered origin's document is supplied in process. It is deliberately
  // absent from the proxy allowlist, so a redirect cannot leak inputs to it.
  const browser = await launchBrowser(chromium, [shopper, cdn]);
  try {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    const denied = await guardCheckoutPage(page, u => u.href === location);
    let violation = false, documents = 0, submissions = 0;
    ctx.on("page", popup => {violation = true; void popup.close()});
    await ctx.routeWebSocket("**/*", route => {violation = true;return route.close()});
    function isPayment(u) {
      return u.origin === shopper && u.pathname === "/checkoutshopper/v1/sessions/" + session.id + "/payments" &&
        u.searchParams.get("clientKey") === key;
    }
    await ctx.route("**/*", async route => {
      const r = route.request(), u = new URL(r.url());
      if (r.isNavigationRequest() && r.frame() === page.mainFrame() && u.href === location && documents++ === 0) {
        return route.fulfill({contentType:"text/html",body:'<!doctype html><meta charset="utf-8"><title>Test ACH submission</title><div id="ach"></div>'});
      }
      const body = r.postData() || "";
      const hasFixture = [knownRouting, knownAccount].some(x => body.includes(x) || u.href.includes(x));
      if (isPayment(u) && r.method() === "POST") {
        let method;
        try {method = JSON.parse(body).paymentMethod} catch {}
        if (method?.type === "ach" && method.accountHolderType === "personal" &&
            method.bankAccountType === "checking" && method.bankLocationId === knownRouting &&
            method.bankAccountNumber === knownAccount) {
          submissions++;
          return route.continue();
        }
      } else if (!hasFixture && !r.isNavigationRequest() && [shopper, cdn].includes(u.origin)) {
        return route.continue();
      }
      violation = true;
      return route.abort();
    });
    await page.goto(location);
    await page.evaluate(async ({ sdk, key, session }) => {
      const script = document.createElement("script");
      script.integrity = "sha384-crX4Byf88JpnQfdUaDuwVOj3qlk5tmSa3rhIalmJIyo1kC49EDIcnNQ9fY5blVUV";
      script.crossOrigin = "anonymous";
      const loaded = new Promise((resolve, reject) => {script.onload = resolve; script.onerror = reject});
      script.src = sdk; document.head.appendChild(script);
      await loaded;
      const { AdyenCheckout, Ach } = window.AdyenWeb;
      const checkout = await AdyenCheckout({environment:"test",countryCode:"US",locale:"en-US",
        clientKey:key,session,analytics:{enabled:false}});
      new Ach(checkout, {hasHolderName:true,enableStoreDetails:false}).mount("#ach");
    }, { sdk, key, session });
    await page.getByRole("combobox").click();
    await page.getByRole("option", {name:"Personal Checking Account"}).click();
    for (const [field, value] of Object.entries({ownerName:"Test Shopper", routingNumber:knownRouting,
      accountNumber:knownAccount, accountNumberVerification:knownAccount})) {
      await page.locator('input[name="' + field + '"]').fill(value);
    }
    const reply = page.waitForResponse(r => isPayment(new URL(r.url())) && r.request().method() === "POST");
    await page.getByRole("button", {name:/pay/i}).click();
    const response = await reply;
    assert.ok(response.ok());
    const {resultCode, action} = await response.json();
    assert.equal(action, undefined);
    assert.ok((refused ? ["Refused"] : ["Authorised","Pending","Received"]).includes(resultCode));
    assert.equal(documents, 1);
    assert.equal(submissions, 1);
    assert.equal(violation || denied(), false);
    assert.equal(page.url(), location);
    assert.equal(ctx.pages().length, 1);
  } finally {await browser.close()}
}
run().catch(() => {process.exitCode = 1});
