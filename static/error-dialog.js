// An error Emissio reports after a form opens as a dialog over the page, so it
// is seen where the form was, not only in the banner at the top of the page.
// The banner stays: it is what a browser without scripts shows.
const banner = document.querySelector("[data-error]");
if (banner && typeof HTMLDialogElement === "function") {
  const d = document.createElement("dialog");
  d.className = "error-dialog";
  const p = document.createElement("p");
  p.textContent = banner.textContent.trim();
  const form = document.createElement("form");
  form.method = "dialog";
  const ok = document.createElement("button");
  ok.className = "btn";
  ok.textContent = "OK";
  form.append(ok);
  d.append(p, form);
  d.addEventListener("click", (e) => { if (e.target === d) d.close(); });
  document.body.append(d);
  d.showModal();
}
