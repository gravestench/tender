const test = require("node:test"),
  assert = require("node:assert/strict"),
  http = require("node:http");
const { chromium } = require(process.env.TENDER_PLAYWRIGHT || "playwright");
const { prepareAndReadState } = require("./sandbox-card-browser.cjs");
async function serve(t, handler) {
  const server = http.createServer(handler);
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => {
    server.closeAllConnections();
    return new Promise((resolve) => server.close(resolve));
  });
  return "http://127.0.0.1:" + server.address().port;
}
async function guarded(t, origins) {
  const guard = require("./browser-guard.cjs");
  const browser = await guard.launchBrowser(chromium, origins);
  t.after(() => browser.close());
  return (await browser.newContext()).newPage();
}
test("follows permitted redirects and denies every redirected destination", async (t) => {
  let visits = 0;
  const other = await serve(t, (_req, res) => {
    visits++;
    res.end("outside");
  });
  const origin = await serve(t, (req, res) => {
    if (req.url === "/allowed") {
      res.end("inside");
      return;
    }
    const to = {
      "/a": "/b",
      "/b": "/allowed",
      "/cross": "/hop",
      "/hop": other + "/outside",
      "/external": "http://external.invalid/resource",
    }[req.url];
    if (to) res.writeHead(302, { Location: to });
    res.end();
  });
  const page = await guarded(t, [origin]);
  await page.goto(origin + "/a");
  assert.equal(page.url(), origin + "/allowed");
  for (const path of ["/cross", "/external"]) {
    const response = await page.goto(origin + path).catch(() => null);
    assert.ok(!response || response.status() >= 400);
  }
  assert.equal(visits, 0);
});
test("qualification reads the component response with the checkout CSP active", async (t) => {
  const origin = await serve(t, (req, res) => {
    if (req.url === "/orders/1/card-state") {
      res.setHeader("Content-Type", "application/json");
      res.end('{"ClientKey":"test_fixture","Card":{"brands":["visa"]}}');
      return;
    }
    if (req.url === "/component") {
      res.setHeader("Content-Type", "text/html");
      res.setHeader("Content-Security-Policy", "connect-src 'self'");
      res.end("<script>fetch('/orders/1/card-state')</script>");
      return;
    }
    res.setHeader("Content-Type", "text/html");
    res.setHeader("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-src 'self'");
    res.end(
      req.url.includes("?")
        ? "<iframe src='/component'></iframe>"
        : "<form><input name='prepare' type='hidden' value='1'><button>Prepare card checkout</button></form>",
    );
  });
  const browser = await chromium.launch({ executablePath: process.env.TENDER_CHROMIUM || undefined });
  t.after(() => browser.close());
  const page = await browser.newPage();
  const order = origin + "/orders/1";
  await page.goto(order);
  const state = await prepareAndReadState(page, order);
  assert.deepEqual(state, { key: "test_fixture", brands: ["visa"] });
});

test("HTTPS redirects cannot escape the tunnel policy or navigate checkout to an approved frame origin", async (t) => {
  const { execFileSync } = require("node:child_process"),
    fs = require("node:fs"),
    os = require("node:os"),
    path = require("node:path");
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "tender-browser-tls-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  execFileSync(
    "openssl",
    [
      "req",
      "-x509",
      "-newkey",
      "rsa:2048",
      "-nodes",
      "-days",
      "1",
      "-subj",
      "/CN=localhost",
      "-keyout",
      path.join(dir, "key"),
      "-out",
      path.join(dir, "cert"),
    ],
    { stdio: "ignore" },
  );
  const tls = { key: fs.readFileSync(path.join(dir, "key")), cert: fs.readFileSync(path.join(dir, "cert")) };
  async function endpoint(handler) {
    const server = require("node:https").createServer(tls, handler);
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    t.after(() => {
      server.closeAllConnections();
      return new Promise((resolve) => server.close(resolve));
    });
    return "https://127.0.0.1:" + server.address().port;
  }
  let externalHits = 0,
    issuerHits = 0;
  const external = await endpoint((_req, res) => {
    externalHits++;
    res.end("outside");
  });
  const issuer = await endpoint((_req, res) => {
    issuerHits++;
    res.end("issuer");
  });
  const store = await endpoint((req, res) => {
    const to = {
      "/allowed": issuer + "/accepted",
      "/blocked": "/hop",
      "/hop": external + "/denied",
      "/external": "https://external.invalid/denied",
      "/issuer": issuer + "/handoff",
    }[req.url];
    if (to) res.writeHead(302, { Location: to });
    res.end();
  });
  const guard = require("./browser-guard.cjs");
  const browser = await guard.launchBrowser(chromium, [store, issuer]);
  t.after(() => browser.close());
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true }); // ephemeral fixture certificates only
  const page = await ctx.newPage();
  await page.goto(store + "/allowed");
  assert.equal(page.url(), issuer + "/accepted");
  for (const u of ["/blocked", "/external"]) await assert.rejects(page.goto(store + u));
  assert.equal(externalHits, 0);
  const before = issuerHits;
  const denied = await guard.guardCheckoutPage(page, (url) => url.origin === store);
  await assert.rejects(page.goto(store + "/issuer"));
  assert.equal(denied(), true);
  assert.equal(issuerHits, before);
});
