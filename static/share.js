// Copy buttons of the invite kit: data-copy holds the text itself,
// data-copy-from names the field that holds it.
for (const b of document.querySelectorAll("[data-share] [data-copy], [data-share] [data-copy-from]")) {
  const label = b.textContent;
  b.addEventListener("click", async () => {
    const from = b.dataset.copyFrom && document.getElementById(b.dataset.copyFrom);
    const text = from ? from.value : b.dataset.copy;
    try {
      await navigator.clipboard.writeText(text);
      b.textContent = "Copied";
    } catch {
      if (from) from.select();
      b.textContent = "Press Ctrl+C to copy";
    }
    setTimeout(() => { b.textContent = label; }, 2500);
  });
}
