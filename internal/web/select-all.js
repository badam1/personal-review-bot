// The review page's decision panel: Select all / Clear all for the findings waiting, and the Post button saying what it
// posts. The button stays hidden without JavaScript, so the page still works as plain checkboxes.
const button = document.getElementById("select-all");
const post = document.getElementById("post");
const boxes = [...document.querySelectorAll('input[type=checkbox][name^="f_"]')];
const reminders = [...document.querySelectorAll('input[type=checkbox][name^="rm_"]')];
const allTicked = () => boxes.every((b) => b.checked);
const count = (n, one, many) => `${n} ${n === 1 ? one : many}`;

const sync = () => {
  if (button) button.textContent = allTicked() ? "Clear all" : "Select all";
  if (!post) return;
  const f = boxes.filter((b) => b.checked).length, r = reminders.filter((b) => b.checked).length;
  const what = [f && count(f, "finding", "findings"), r && count(r, "reminder", "reminders")].filter(Boolean);
  post.textContent = what.length ? "Post " + what.join(" and ") : "Post selected";
};

if (button) {
  button.addEventListener("click", () => {
    const tick = !allTicked();
    boxes.forEach((b) => { b.checked = tick; });
    sync();
  });
  button.hidden = false;
}
[...boxes, ...reminders].forEach((b) => b.addEventListener("change", sync));
sync();
