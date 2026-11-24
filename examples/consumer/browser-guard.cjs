// Chromium's request routing can skip subsequent HTTP redirects. This local
// proxy checks the actual outbound authority, including HTTPS CONNECT on
// redirected subresources in out-of-process frames. Chromium owns end-to-end TLS.
const http = require("node:http");
const net = require("node:net");

async function launchBrowser(chromium, origins) {
  const allowed = new Set(origins);
  function permitted(url) {
    return !url.username && !url.password && allowed.has(url.origin);
  }
  const sockets = new Set();
  const proxy = http.createServer((req, res) => {
    let url;
    try {
      url = new URL(req.url);
    } catch {
      res.writeHead(400).end();
      return;
    }
    if (url.protocol !== "http:" || !permitted(url)) {
      res.writeHead(403).end();
      return;
    }
    const headers = { ...req.headers, host: url.host };
    delete headers["proxy-authorization"];
    delete headers["proxy-connection"];
    const outgoing = http.request(url, { method: req.method, headers }, (upstream) => {
      if (upstream.statusCode >= 300 && upstream.statusCode < 400 && upstream.headers.location) {
        let approved = false;
        try {
          approved = permitted(new URL(upstream.headers.location, url));
        } catch {}
        if (!approved) {
          upstream.resume();
          res.writeHead(403).end();
          return;
        }
      }
      res.writeHead(upstream.statusCode, upstream.headers);
      upstream.pipe(res);
    });
    outgoing.on("error", () => res.destroy());
    req.pipe(outgoing);
  });
  proxy.on("connection", (socket) => {
    sockets.add(socket);
    socket.once("close", () => sockets.delete(socket));
  });
  proxy.on("connect", (req, client, head) => {
    let url;
    try {
      url = new URL("https://" + req.url);
    } catch {
      client.destroy();
      return;
    }
    if (!permitted(url) || url.pathname !== "/" || url.search || url.hash) {
      client.end("HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n");
      return;
    }
    const upstream = net.connect(Number(url.port || 443), url.hostname);
    upstream.on("error", () => client.destroy());
    client.on("error", () => upstream.destroy());
    client.on("close", () => upstream.destroy());
    upstream.on("connect", () => {
      client.write("HTTP/1.1 200 Connection Established\r\n\r\n");
      if (head.length) upstream.write(head);
      client.pipe(upstream).pipe(client);
    });
  });
  await new Promise((resolve, reject) => {
    proxy.once("error", reject);
    proxy.listen(0, "127.0.0.1", resolve);
  });
  let browser;
  const close = async () => {
    try {
      if (browser) await browser.close();
    } finally {
      for (const socket of sockets) socket.destroy();
      await new Promise((resolve) => proxy.close(resolve));
    }
  };
  try {
    browser = await chromium.launch({
      executablePath: process.env.TENDER_CHROMIUM || undefined,
      proxy: { server: "http://127.0.0.1:" + proxy.address().port, bypass: "<-loopback>" },
      args: ["--disable-background-networking", "--disable-quic", "--disable-http2"],
    });
    return { newContext: (opts) => browser.newContext({ ...opts, serviceWorkers: "block" }), close };
  } catch (error) {
    await close();
    throw error;
  }
}

// Every top document request and redirect Location must also obey the current
// checkout URL policy, even if the destination is trusted for nested fields.
async function guardCheckoutPage(page, permitted) {
  const session = await page.context().newCDPSession(page);
  const { frameTree } = await session.send("Page.getFrameTree");
  let denied = false;
  session.on("Fetch.requestPaused", async (event) => {
    const { requestId, request, responseHeaders, responseStatusCode, frameId } = event;
    let approve = true;
    if (frameId === frameTree.frame.id) {
      try {
        approve = permitted(new URL(request.url));
        const location = responseHeaders?.find((h) => h.name.toLowerCase() === "location");
        if (approve && responseStatusCode >= 300 && responseStatusCode < 400 && location)
          approve = permitted(new URL(location.value, request.url));
      } catch {
        approve = false;
      }
    }
    if (!approve) denied = true;
    try {
      if (approve) await session.send("Fetch.continueRequest", { requestId });
      else await session.send("Fetch.failRequest", { requestId, errorReason: "BlockedByClient" });
    } catch {
      /* The target may close during teardown. */
    }
  });
  await session.send("Fetch.enable", {
    patterns: ["Request", "Response"].map((requestStage) => ({ resourceType: "Document", requestStage })),
  });
  return () => denied;
}
module.exports = { launchBrowser, guardCheckoutPage };
