const checkout = document.querySelector("main");
const message = document.querySelector("#message");
const controls = [...checkout.querySelectorAll("button")];

function render(state) {
  checkout.dataset.state = state;
  if (!controls.length) return;
  controls[0].hidden = state !== "unpaid" && state !== "cancelled";
  checkout.querySelector("section").hidden = state !== "requires_action";
  message.textContent = ({
    unavailable: "PayPal is unavailable for this checkout.",
    requires_action: "Approve or cancel the synthetic payment.",
    cancelled: "Approval cancelled. No payment collected.",
    captured: "Payment captured.",
    unpaid: "Order unpaid.",
  })[state] || state;
}
for (const button of controls) {
  button.addEventListener("click", async () => {
    controls.forEach(control => control.disabled = true);
    let nextFocus = message;
    try {
      async function submit(step) {
        const response = await fetch(location.pathname + "/wallet/" + step, {
          method: "POST",
          headers: {"Content-Type": "application/x-www-form-urlencoded"},
          body: new URLSearchParams({csrf: checkout.dataset.csrf}),
        });
        if (!response.ok) throw new Error("wallet unavailable");
        return (await response.json()).status;
      }
      const status = await submit(button.dataset.step);
      const next = status === "approved" ? await submit("confirm") : status;
      render(next);
      if (next === "requires_action") nextFocus = checkout.querySelector('[data-step="approve"]');
    } catch {
      render("unavailable");
    } finally {
      controls.forEach(control => control.disabled = false);
      nextFocus.focus();
    }
  });
}
render(checkout.dataset.state);
