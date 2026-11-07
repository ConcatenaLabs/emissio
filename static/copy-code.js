// A copy button under each receive code Emissio shows on a task page.
for (const el of document.querySelectorAll("[data-emissio-code]")) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "btn ghost small u-margin-top-8rem";
  b.textContent = "Copy code";
  b.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(el.textContent.trim());
      b.textContent = "Copied";
    } catch {
      b.textContent = "Select the code and copy it by hand";
    }
    setTimeout(() => { b.textContent = "Copy code"; }, 2500);
  });
  el.after(b);
}
