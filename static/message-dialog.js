// What Emissio says after a form (an error, a confirmation, a credit) opens
// as a dialog over the page, where the person is looking, not only in the
// banner at the top of the page. The banner stays: it is what a browser
// without scripts shows. An error comes first when there are both.
const banner = document.querySelector("[data-message][data-error]") || document.querySelector("[data-message]");
if (banner && typeof HTMLDialogElement === "function") {
  const d = document.createElement("dialog");
  d.className = "message-dialog" + (banner.hasAttribute("data-error") ? " err" : "");
  const p = document.createElement("p");
  p.textContent = banner.textContent.trim();
  const form = document.createElement("form");
  form.method = "dialog";
  const ok = document.createElement("button");
  ok.className = "btn";
  ok.textContent = banner.hasAttribute("data-credited") ? "Great!" : "OK";
  form.append(ok);
  d.append(p, form);
  d.addEventListener("click", (e) => { if (e.target === d) d.close(); });
  document.body.append(d);
  d.showModal();
}
