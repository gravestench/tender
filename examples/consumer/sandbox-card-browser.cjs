// Uses published Adyen TEST card fixtures only, in Adyen-owned iframe contexts.
// Never log URLs (tickets), network bodies, form values or screenshots.
const assert = require("node:assert/strict");
const {chromium} = require(process.env.TENDER_PLAYWRIGHT || "playwright");
const origin = process.env.TENDER_ORIGIN;
const shopper = "https://checkoutshopper-test.adyen.com";
const cdn = "https://checkoutshopper-test.cdn.adyen.com";
const issuer = process.env.TENDER_ADYEN_TEST_ACS_ORIGIN || shopper;
const scenario = process.env.TENDER_CASE;
const cards = {visa_challenge: "4212345678910006", mc_frictionless: "5201281505129736", mc_purchase: "5555444433331111"};
(async () => {
  const browser = await chromium.launch({executablePath: process.env.TENDER_CHROMIUM || undefined});
  try {
    const ctx = await browser.newContext({serviceWorkers:"block", viewport: scenario === "mc_frictionless" ? {width:390,height:844} : {width:1200,height:900}});
    const allowed = new Set([origin,shopper,cdn,issuer]);
    const page = await ctx.newPage();
    let unexpected = false, orderURL = "", challengeSeen = false;
    ctx.on("page", popup => {unexpected = true; void popup.close();});
    await ctx.routeWebSocket("**/*", route => route.close());
    await ctx.route("**/*", route => {
      const req = route.request(), u = new URL(req.url());
      const outer = req.isNavigationRequest() && req.frame() === page.mainFrame();
      if (!allowed.has(u.origin) || outer && (u.origin !== origin || orderURL && u.href !== orderURL)) {
        unexpected = true;
        return route.abort();
      }
      return route.continue();
    });
    await page.goto(process.env.TENDER_TICKET);
    orderURL = page.url();
    assert.equal(new URL(orderURL).origin, origin);
    assert.match(new URL(orderURL).pathname, /^\/orders\/[\da-f-]+$/);
    await page.getByRole("button",{name:"Prepare card checkout"}).click();
    const host = page.frameLocator("#card-frame");
    const snapshot = await page.evaluate(async () => {
      const res = await fetch(location.pathname+"/card-state");
      const {ClientKey,Card} = await res.json();
      return {key:ClientKey, brands:Card.brands};
    });
    assert.equal(snapshot.key,process.env.TENDER_EXPECTED_CLIENT_KEY);
    assert.deepEqual(snapshot.brands,[scenario === "visa_challenge" ? "visa" : "mc"]);
    // Resolve and inspect each secured field's actual origin before filling it.
    for (const [field,value] of [["encryptedCardNumber",cards[scenario]],["encryptedExpiryDate","0330"],["encryptedSecurityCode","737"]]) {
      const frameElement = host.locator(`[data-cse="${field}"] iframe`);
      await frameElement.waitFor();
      const frame = await (await frameElement.elementHandle()).contentFrame();
      await frame.waitForURL(url => url.origin === shopper);
      await frame.locator("input:visible").fill(value);
    }
    await host.getByRole("textbox",{name:/name on card|cardholder name/i}).fill("Test Shopper");
    await host.getByRole("button",{name:/pay/i}).click();
    if (scenario === "visa_challenge") {
      // Fill only the configured test issuer's embedded challenge.
      const deadline = Date.now()+30000;
      while (Date.now()<deadline && !challengeSeen) {
        for (const frame of page.frames().filter(f => f.parentFrame() && /^https:/.test(f.url()) && new URL(f.url()).origin===issuer)) {
          const password=frame.locator('input[type="password"]:visible');
          if (await password.count()!==1) continue;
          await password.fill("password");
          await frame.getByRole("button",{name:/submit|confirm|continue/i}).click();
          challengeSeen = true;
        }
        if (!challengeSeen) await new Promise(resolve=>setTimeout(resolve,100));
      }
      assert.ok(challengeSeen);
    }
    if (scenario === "mc_frictionless") {
      await page.getByText("This payment needs review.",{exact:false}).waitFor();
      assert.equal(await page.locator("#card-frame").count(),0);
    } else {
      await page.getByText("authorized",{exact:true}).waitFor();
    }
    assert.equal(page.url(),orderURL);
    assert.equal(ctx.pages().length,1);
    assert.equal(unexpected,false);
  } finally { await browser.close(); }
})().catch(()=>{process.exitCode=1;});
