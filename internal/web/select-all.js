// Select all / Clear all for the findings waiting on the review page. The button stays hidden without JavaScript,
// so the page still works as plain checkboxes.
const button = document.getElementById("select-all");
if (button) {
  const boxes = [...document.querySelectorAll('input[type=checkbox][name^="f_"]')];
  const allTicked = () => boxes.every((b) => b.checked);
  const sync = () => { button.textContent = allTicked() ? "Clear all" : "Select all"; };
  button.addEventListener("click", () => {
    const tick = !allTicked();
    boxes.forEach((b) => { b.checked = tick; });
    sync();
  });
  boxes.forEach((b) => b.addEventListener("change", sync));
  button.hidden = false;
  sync();
}
