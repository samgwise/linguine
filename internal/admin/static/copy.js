// copy.js — copy-to-clipboard buttons on the key-reveal pages. The raw
// secret is read from the reveal element's text content, so the key is never
// embedded in a script or attribute and the dashboard CSP stays
// `script-src 'self'`. Registered as a delegated listener so it keeps working
// across hx-boosted navigations that swap out the body.
document.addEventListener('click', function (e) {
  var btn = e.target.closest('button[data-copy]');
  if (!btn) return;
  var target = document.querySelector(btn.getAttribute('data-copy'));
  var original = btn.textContent;
  var report = function (msg) {
    btn.textContent = msg;
    btn.disabled = true;
    setTimeout(function () {
      btn.textContent = original;
      btn.disabled = false;
    }, 1500);
  };
  if (!navigator.clipboard) {
    report('Copy unsupported');
    return;
  }
  navigator.clipboard.writeText(target ? target.textContent.trim() : '').then(
    function () { report('Copied'); },
    function () { report('Copy failed'); }
  );
});
