const cardFrame = document.getElementById("card-frame");
window.addEventListener("message", event => {
  if (cardFrame && event.origin === location.origin && event.source === cardFrame.contentWindow &&
      event.data === "tender:refresh-order") location.reload();
});
