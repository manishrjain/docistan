// Progressive enhancement only: without this file the form still works as a
// plain file picker and submit button.
const zone = document.getElementById("dropzone");
const input = document.getElementById("file-input");
const form = document.getElementById("upload-form");

if (zone && input && form) {
  for (const type of ["dragenter", "dragover", "dragleave", "drop"]) {
    zone.addEventListener(type, (e) => {
      e.preventDefault();
      e.stopPropagation();
    });
  }
  zone.addEventListener("dragover", () => zone.classList.add("over"));
  zone.addEventListener("dragleave", () => zone.classList.remove("over"));
  zone.addEventListener("drop", (e) => {
    zone.classList.remove("over");
    if (e.dataTransfer.files.length) {
      input.files = e.dataTransfer.files;
      form.submit();
    }
  });
}
