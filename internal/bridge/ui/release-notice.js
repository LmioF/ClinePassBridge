/* Release notice state is isolated from its markup and scoped to this console. */
const releaseNoticeModule = (() => {
  'use strict';
  const button = document.getElementById('releaseNoticeButton');
  const dialog = document.getElementById('releaseNoticeDialog');
  const versionLabel = document.getElementById('releaseNoticeVersion');
  const dismissed = new Set();
  const presented = new Set();
  const service = location.origin + location.pathname.replace(/\/+$/, '');
  let currentVersion = '';
  let displayedVersion = '';
  let active = false;
  let retryQueued = false;

  const storageKey = version => `clinepassbridge:release-notice:${service}:${encodeURIComponent(version)}`;
  function wasDismissed(version) {
    if (dismissed.has(version)) return true;
    try {
      if (localStorage.getItem(storageKey(version)) === '1') {
        dismissed.add(version);
        return true;
      }
    } catch { /* Private browsing may block storage; page state still works. */ }
    return false;
  }
  function render() {
    const labelVersion = active ? displayedVersion : currentVersion;
    versionLabel.textContent = labelVersion ? `v${labelVersion}` : '更新公告';
    button.toggleAttribute('data-unread', !!currentVersion && !wasDismissed(currentVersion));
  }
  function open(manual = false) {
    if (active || dialog.open) return;
    if (!manual) {
      if (!currentVersion || presented.has(currentVersion) || wasDismissed(currentVersion)) return;
      if (document.querySelector('dialog[open]')) return;
    }
    // Capture the version actually presented. A version fetched later must not
    // be acknowledged by closing a notice opened before authentication.
    displayedVersion = currentVersion;
    dialog.showModal();
    active = true;
    if (displayedVersion) presented.add(displayedVersion);
    render();
  }
  function retry() {
    if (retryQueued) return;
    retryQueued = true;
    queueMicrotask(() => {
      retryQueued = false;
      open();
    });
  }
  button.addEventListener('click', () => open(true));
  document.getElementById('releaseNoticeDismiss').addEventListener('click', () => dialog.close());
  document.getElementById('releaseNoticeSettings').addEventListener('click', () => {
    setTab('config');
    dialog.close();
  });
  dialog.addEventListener('close', () => {
    if (!active) return;
    active = false;
    if (displayedVersion) {
      dismissed.add(displayedVersion);
      try { localStorage.setItem(storageKey(displayedVersion), '1'); } catch { /* Keep page-local acknowledgement. */ }
    }
    displayedVersion = '';
    render();
    retry();
  });
  // close does not bubble. Capture it so authentication and other dialogs can
  // finish their own close handlers before the pending announcement is shown.
  document.addEventListener('close', retry, true);
  window.addEventListener('storage', event => {
    if (!currentVersion || event.key !== storageKey(currentVersion) || event.newValue !== '1') return;
    dismissed.add(currentVersion);
    render();
  });
  render();
  return {
    update(version) {
      if (typeof version !== 'string' || !version.trim()) return;
      currentVersion = version.trim();
      render();
      open();
    },
  };
})();

function updateReleaseNotice(version) {
  releaseNoticeModule.update(version);
}
