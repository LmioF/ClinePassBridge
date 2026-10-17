const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const source = fs.readFileSync(path.join(__dirname, '../internal/bridge/ui/release-notice.js'), 'utf8');

function page({ store = new Map(), blockedStorage = false, pathname = '/v0/resource/plugins/clinepassbridge/console', authOpen = false } = {}) {
  const pending = [];
  const nodes = new Map();
  const documentEvents = new Map();
  const windowEvents = new Map();
  const tabs = [];
  class Node {
    constructor(id, open = false) {
      this.id = id;
      this.open = open;
      this.events = new Map();
      this.attributes = new Set();
      this.openCount = 0;
    }
    addEventListener(name, fn) {
      const handlers = this.events.get(name) || [];
      handlers.push(fn);
      this.events.set(name, handlers);
    }
    emit(name, event = {}) {
      for (const fn of this.events.get(name) || []) fn(event);
    }
    toggleAttribute(name, enabled) { enabled ? this.attributes.add(name) : this.attributes.delete(name); }
    showModal() { this.open = true; this.openCount++; }
    close() {
      if (!this.open) return;
      this.open = false;
      for (const fn of documentEvents.get('close') || []) fn({ target: this });
      this.emit('close');
    }
    escape() {
      const event = { defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } };
      this.emit('cancel', event);
      if (!event.defaultPrevented) this.close();
    }
  }
  for (const id of ['releaseNoticeButton', 'releaseNoticeDialog', 'releaseNoticeVersion', 'releaseNoticeDismiss', 'releaseNoticeSettings', 'authDialog']) {
    nodes.set(id, new Node(id, id === 'authDialog' && authOpen));
  }
  const localStorage = {
    getItem(key) { if (blockedStorage) throw Error('blocked'); return store.get(key) ?? null; },
    setItem(key, value) { if (blockedStorage) throw Error('blocked'); store.set(key, value); },
  };
  const context = vm.createContext({
    document: {
      getElementById: id => nodes.get(id),
      querySelector: selector => selector === 'dialog[open]' && [...nodes.values()].find(node => node.id.endsWith('Dialog') && node.open),
      addEventListener(name, fn) {
        const handlers = documentEvents.get(name) || [];
        handlers.push(fn);
        documentEvents.set(name, handlers);
      },
    },
    window: { addEventListener: (name, fn) => windowEvents.set(name, fn) },
    location: { origin: 'https://cpa.example', pathname },
    localStorage,
    queueMicrotask: fn => pending.push(fn),
    setTab: name => tabs.push(name),
  });
  vm.runInContext(source, context);
  const flush = () => { while (pending.length) pending.shift()(); };
  const button = nodes.get('releaseNoticeButton');
  const dialog = nodes.get('releaseNoticeDialog');
  return {
    store, nodes, button, dialog, tabs, flush,
    update: version => context.updateReleaseNotice(version),
    click: id => { nodes.get(id).emit('click'); flush(); },
    unread: () => button.attributes.has('data-unread'),
    storage: event => windowEvents.get('storage')(event),
  };
}

test('first visit presents once; dismissal persists across refresh; a new version presents again', () => {
  const first = page();
  first.update('0.18.0');
  assert.equal(first.dialog.openCount, 1);
  assert.equal(first.unread(), true);
  first.update('0.18.0');
  assert.equal(first.dialog.openCount, 1);
  first.click('releaseNoticeDismiss');
  assert.equal(first.unread(), false);
  first.update('0.18.0');
  assert.equal(first.dialog.openCount, 1);

  const refreshed = page({ store: first.store });
  refreshed.update('0.18.0');
  assert.equal(refreshed.dialog.open, false);
  refreshed.update('0.19.0');
  assert.equal(refreshed.dialog.open, true);
  assert.equal(refreshed.nodes.get('releaseNoticeVersion').textContent, 'v0.19.0');
  const otherService = page({ store: first.store, pathname: '/other/v0/resource/plugins/clinepassbridge/console' });
  otherService.update('0.18.0');
  assert.equal(otherService.dialog.open, true);
});

test('manual bell reopens read notices; settings navigates and acknowledges; Escape also acknowledges', () => {
  const view = page();
  view.update('1.0');
  view.dialog.escape();
  view.flush();
  assert.equal(view.unread(), false);
  assert.equal(view.store.size, 1);
  view.click('releaseNoticeButton');
  assert.equal(view.dialog.open, true);
  view.click('releaseNoticeSettings');
  assert.deepEqual(view.tabs, ['config']);
  assert.equal(view.dialog.open, false);
  const refreshed = page({ store: view.store });
  refreshed.update('1.0');
  assert.equal(refreshed.dialog.open, false);
});

test('automatic notice waits for authentication and any other open dialog to close', () => {
  const view = page({ authOpen: true });
  view.update('1.0');
  assert.equal(view.dialog.open, false);
  view.nodes.get('authDialog').close();
  assert.equal(view.dialog.open, false, 'notice waits for all close handlers');
  view.flush();
  assert.equal(view.dialog.open, true);
  assert.equal(view.dialog.openCount, 1);
});

test('a manual notice opened without version does not acknowledge a subsequently fetched version', () => {
  const view = page();
  view.click('releaseNoticeButton');
  assert.equal(view.dialog.open, true);
  assert.equal(view.nodes.get('releaseNoticeVersion').textContent, '更新公告');
  view.update('1.0');
  view.click('releaseNoticeDismiss');
  assert.equal(view.store.size, 0);
  assert.equal(view.unread(), true);
  assert.equal(view.dialog.openCount, 2, 'known version receives its own presentation');
  view.dialog.escape();
  view.flush();
  assert.equal(view.store.size, 1);
});

test('blocked storage still acknowledges within this page and cross-tab acknowledgement cancels a pending notice', () => {
  const blocked = page({ blockedStorage: true });
  blocked.update('1.0');
  blocked.click('releaseNoticeDismiss');
  blocked.update('1.0');
  assert.equal(blocked.dialog.openCount, 1);
  assert.equal(blocked.unread(), false);

  const first = page();
  first.update('1.0');
  first.click('releaseNoticeDismiss');
  const [key, value] = [...first.store][0];
  const pending = page({ authOpen: true });
  pending.update('1.0');
  pending.storage({ key, newValue: value });
  pending.nodes.get('authDialog').close();
  pending.flush();
  assert.equal(pending.dialog.open, false);
  assert.equal(pending.unread(), false);
});
