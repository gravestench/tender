"use strict";
(async () => {
  const notice = document.getElementById("notice");
  const mount = document.getElementById("card");
  function stop() {
    mount.replaceChildren();
    notice.textContent = "Secure checkout is unavailable. Refresh the order to check its status.";
  }
  if (window.top === window.self) { stop(); return; }
  function refresh() { window.parent.postMessage("tender:refresh-order", location.origin); }
  const stateURL = new URL("card-state", location.href);
  const confirmURL = new URL("confirm", location.href);
  let state, timer;
  async function getState() {
    const response = await fetch(stateURL, {cache: "no-store", redirect: "error"});
    if (!response.ok) throw new Error();
    state = await response.json();
    const remaining = Date.parse(state.ExpiresAt) - Date.now();
    if (!(remaining > 0)) throw new Error();
    clearTimeout(timer);
    timer = setTimeout(stop, remaining);
    return state.Card;
  }
  function challenge(card) {
    const action = card.action;
    if (card.type !== "threeDS2" || action?.type !== "threeDS2" || action.subtype !== "challenge" ||
        action.paymentMethodType !== "scheme" || !action.token || !action.authorisationToken) throw new Error();
    return action;
  }
  const project = (source, keys) => Object.fromEntries(keys.filter(k => source[k] !== undefined).map(k => [k, source[k]]));
  let sdkBrand;
  async function send(data, actions) {
    try {
      if (!(Date.parse(state.ExpiresAt) > Date.now())) throw new Error();
      const reply = await fetch(confirmURL, {
        method: "POST", redirect: "error",
        headers: {"Content-Type": "application/json", "X-CSRF-Token": state.CSRF, "X-Action-Version": String(state.Version)},
        body: JSON.stringify(data),
      });
      if (!reply.ok) throw new Error();
      const {resultCode} = await reply.json();
      if (resultCode === "ChallengeShopper") {
        actions.resolve({resultCode, action: challenge(await getState())});
      } else if (["Authorised", "Refused", "Cancelled", "Pending", "Received"].includes(resultCode)) {
        actions.resolve({resultCode});
        refresh();
      } else {
        actions.reject();
        refresh();
      }
    } catch {
      actions.reject();
      stop();
    }
  }
  try {
    const card = await getState();
    const {AdyenCheckout, Card} = window.AdyenWeb;
    const config = {
      environment: "test", clientKey: state.ClientKey, countryCode: "US", locale: "en-US",
      amount: {value: state.Amount.Minor, currency: state.Amount.Currency},
      analytics: {enabled: false},
      onSubmit: (componentState, component, actions) => {
        if (!componentState.isValid || card.type !== "card") { actions.reject(); return; }
        const {paymentMethod, browserInfo} = componentState.data;
        const method = project(paymentMethod, ["type", "brand", "holderName", "encryptedCardNumber", "encryptedExpiryMonth", "encryptedExpiryYear", "encryptedSecurityCode", "checkoutAttemptId"]);
        method.brand = method.brand || sdkBrand;
        const jwe = /^eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/;
        const encrypted = [method.encryptedCardNumber, method.encryptedExpiryMonth, method.encryptedExpiryYear, method.encryptedSecurityCode];
        if (method.type !== "scheme" || !card.brands.includes(method.brand) ||
            !encrypted.every(v => typeof v === "string" && v.length <= 15000 && jwe.test(v))) {
          actions.reject(); stop(); return;
        }
        const info = project(browserInfo, ["acceptHeader", "colorDepth", "javaEnabled", "javaScriptEnabled", "language", "screenHeight", "screenWidth", "timeZoneOffset", "userAgent"]);
        return send({paymentMethod: method, browserInfo: info}, actions);
      },
      onAdditionalDetails: (componentState, component, actions) => {
        const result = componentState.data?.details?.threeDSResult;
        if (typeof result !== "string" || !result) { actions.reject(); stop(); return; }
        return send({details: {threeDSResult: result}}, actions);
      },
      onError: stop,
    };
    const checkout = await AdyenCheckout(config);
    if (card.type === "card" && card.brands.length === 1 && ["visa", "mc"].includes(card.brands[0])) {
      new Card(checkout, {
        brands: card.brands, hasHolderName: true, holderNameRequired: true, showPayButton: true,
        onBrand: ({brand}) => { sdkBrand = brand; },
      }).mount(mount);
    } else {
      checkout.createFromAction(challenge(card)).mount(mount);
    }
    notice.textContent = "";
  } catch { stop(); }
})();
