import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("./state-model.html", import.meta.url), "utf8");
const modelSource = source.match(
  /const PersonalDesktopModel = \(\(\) => \{[\s\S]*?\n    \}\)\(\);/,
)?.[0];
assert.ok(modelSource, "PersonalDesktopModel source was not found");

const context = vm.createContext({ structuredClone });
vm.runInContext(`${modelSource}\nglobalThis.model = PersonalDesktopModel;`, context);
const model = context.model;

const ownerRequest = {
  type: "REQUEST_ACCESS",
  sessionId: "human",
  actor: "Human",
  ownerKey: "owner-a",
};

function reduce(actions) {
  return actions.reduce((state, action) => model.reduce(state, action), model.initialState());
}

const confirmComputeStopped = (state) => model.reduce(state, {
  type: "DELETE_COMPUTE_STOPPED",
  diskId: state.desktop.disk?.id,
  vmiAbsent: true,
});
const confirmVMDetached = (state) => model.reduce(state, {
  type: "DELETE_VM_DETACHED",
  diskId: state.desktop.disk?.id,
  vmStorageReferenceAbsent: true,
  volumeDetached: true,
});
const confirmStorageDeleted = (state) => model.reduce(state, {
  type: "DELETE_SUCCEEDED",
  diskId: state.desktop.disk?.id,
  dataVolumeAbsent: true,
  pvcAbsent: true,
});

function assertInvariants(state) {
  const failed = model.assertions(state).filter((item) => !item.pass);
  assert.equal(failed.length, 0, failed.map((item) => item.label).join(", "));
}

const reachable = {
  Absent: [],
  Provisioning: [ownerRequest],
  Stopped: [ownerRequest, { type: "PROVISIONED" }],
  Starting: [ownerRequest, { type: "PROVISIONED" }, { type: "START" }],
  Ready: [ownerRequest, { type: "PROVISIONED" }, { type: "START" }, { type: "READY" }],
  Recovering: [
    ownerRequest,
    { type: "PROVISIONED" },
    { type: "START" },
    { type: "READY" },
    { type: "VM_RESTART" },
  ],
};

test("DISABLE preserves phase invariants from every non-deletion phase", () => {
  for (const [phase, prefix] of Object.entries(reachable)) {
    const before = reduce(prefix);
    assert.equal(before.desktop.phase, phase);
    const after = model.reduce(before, { type: "DISABLE" });
    assert.equal(after.desktop.enabled, false);
    assert.equal(after.desktop.phase, phase === "Absent" ? "Absent" : "Stopped");
    assert.equal(Boolean(after.desktop.disk), phase !== "Absent");
    assertInvariants(after);

    const startAttempt = model.reduce(after, { type: "START" });
    assert.equal(startAttempt.desktop.phase, after.desktop.phase);
  }
});

test("disable cannot regress a deletion phase", () => {
  const ready = reduce(reachable.Ready);
  const requested = model.reduce(ready, { type: "DELETE_REQUEST" });
  const computeStopped = confirmComputeStopped(requested);
  const vmDetached = confirmVMDetached(computeStopped);
  const deleting = model.reduce(vmDetached, { type: "DELETE_BEGIN" });
  const blocked = model.reduce(deleting, { type: "DELETE_FAILED" });

  for (const state of [requested, computeStopped, vmDetached, deleting, blocked]) {
    const after = model.reduce(state, { type: "DISABLE" });
    assert.equal(after.desktop.phase, state.desktop.phase);
    assert.equal(after.desktop.deletionPhase, state.desktop.deletionPhase);
    assertInvariants(after);
  }
});

test("delete completion requires VMI stop and VM detach before storage cleanup", () => {
  const ready = reduce(reachable.Ready);
  const requested = model.reduce(ready, { type: "DELETE_REQUEST" });
  assert.equal(requested.desktop.phase, "Ready");
  assert.equal(requested.desktop.deletionPhase, "Requested");
  assert.equal(requested.desktop.vmPresent, true);
  assert.notEqual(requested.desktop.disk, null);

  const prematureStorageDelete = model.reduce(requested, { type: "DELETE_BEGIN" });
  assert.equal(prematureStorageDelete.desktop.deletionPhase, "Requested");
  assert.match(prematureStorageDelete.rejected.at(-1), /cannot begin storage cleanup/);

  const wrongDiskEvidence = model.reduce(requested, {
    type: "DELETE_COMPUTE_STOPPED",
    diskId: "another-disk",
    vmiAbsent: true,
  });
  assert.equal(wrongDiskEvidence.desktop.deletionPhase, "Requested");
  assert.match(wrongDiskEvidence.rejected.at(-1), /current disk ID/);

  const computeStopped = confirmComputeStopped(requested);
  assert.equal(computeStopped.desktop.phase, "Stopped");
  assert.equal(computeStopped.desktop.deletionPhase, "ComputeStopped");
  assert.equal(computeStopped.desktop.vmPresent, true);

  const stillAttached = model.reduce(computeStopped, { type: "DELETE_BEGIN" });
  assert.equal(stillAttached.desktop.deletionPhase, "ComputeStopped");
  const vmDetached = confirmVMDetached(computeStopped);
  assert.equal(vmDetached.desktop.phase, "Absent");
  assert.equal(vmDetached.desktop.deletionPhase, "VMDetached");
  assert.equal(vmDetached.desktop.vmPresent, false);

  const deleting = model.reduce(vmDetached, { type: "DELETE_BEGIN" });
  assert.equal(deleting.desktop.phase, "Absent");
  assert.equal(deleting.desktop.deletionPhase, "DeletingStorage");
  assert.notEqual(deleting.desktop.disk, null);

  const partialStorageEvidence = model.reduce(deleting, {
    type: "DELETE_SUCCEEDED",
    diskId: deleting.desktop.disk.id,
    dataVolumeAbsent: true,
    pvcAbsent: false,
  });
  assert.equal(partialStorageEvidence.desktop.deletionPhase, "DeletingStorage");
  assert.notEqual(partialStorageEvidence.desktop.disk, null);
  assert.match(partialStorageEvidence.rejected.at(-1), /DataVolume\/PVC absence/);

  const deleted = confirmStorageDeleted(deleting);
  assert.equal(deleted.desktop.phase, "Absent");
  assert.equal(deleted.desktop.deletionPhase, "Deleted");
  assert.equal(deleted.desktop.disk, null);
  assertInvariants(deleted);

  const duplicate = model.reduce(deleted, {
    type: "DELETE_SUCCEEDED",
    diskId: "already-deleted",
    dataVolumeAbsent: true,
    pvcAbsent: true,
  });
  assert.equal(duplicate.desktop.deletionPhase, "Deleted");
  assert.equal(duplicate.desktop.disk, null);
  assertInvariants(duplicate);

  const denied = model.reduce(duplicate, ownerRequest);
  assert.equal(denied.desktop.deletionPhase, "Deleted");
  assert.match(denied.rejected.at(-1), /desktop access disabled/);
  assertInvariants(denied);
});

test("delete failure retains the existing disk and can retry cleanup", () => {
  const ready = reduce(reachable.Ready);
  const diskId = ready.desktop.disk.id;
  const requested = model.reduce(ready, { type: "DELETE_REQUEST" });
  const computeStopped = confirmComputeStopped(requested);
  const vmDetached = confirmVMDetached(computeStopped);
  const deleting = model.reduce(vmDetached, { type: "DELETE_BEGIN" });
  const blocked = model.reduce(deleting, { type: "DELETE_FAILED" });
  assert.equal(blocked.desktop.deletionPhase, "DeletionBlocked");
  assert.equal(blocked.desktop.deletionBlockedAt, "DeleteStorage");
  assert.equal(blocked.desktop.vmPresent, false);
  assert.equal(blocked.desktop.disk.id, diskId);
  assertInvariants(blocked);

  const retrying = model.reduce(blocked, { type: "DELETE_BEGIN" });
  assert.equal(retrying.desktop.deletionPhase, "DeletingStorage");
  assert.equal(retrying.desktop.disk.id, diskId);
  assertInvariants(retrying);
});

test("compute stop and VM detach failures stay distinct from storage failure", () => {
  const requested = model.reduce(reduce(reachable.Ready), { type: "DELETE_REQUEST" });
  const stopBlocked = model.reduce(requested, { type: "DELETE_COMPUTE_FAILED" });
  assert.equal(stopBlocked.desktop.deletionPhase, "DeletionBlocked");
  assert.equal(stopBlocked.desktop.deletionBlockedAt, "StopCompute");
  assert.equal(stopBlocked.desktop.phase, "Ready");
  assert.equal(stopBlocked.desktop.vmPresent, true);
  assert.notEqual(stopBlocked.desktop.disk, null);
  assertInvariants(stopBlocked);

  const computeStopped = confirmComputeStopped(stopBlocked);
  const vmBlocked = model.reduce(computeStopped, { type: "DELETE_VM_FAILED" });
  assert.equal(vmBlocked.desktop.deletionPhase, "DeletionBlocked");
  assert.equal(vmBlocked.desktop.deletionBlockedAt, "DeleteVM");
  assert.equal(vmBlocked.desktop.phase, "Stopped");
  assert.equal(vmBlocked.desktop.vmPresent, true);
  assertInvariants(vmBlocked);

  const detached = confirmVMDetached(vmBlocked);
  assert.equal(detached.desktop.deletionPhase, "VMDetached");
  assert.equal(detached.desktop.vmPresent, false);
  assertInvariants(detached);
});

test("deleting an absent desktop completes without inventing a disk", () => {
  const deleted = model.reduce(model.initialState(), { type: "DELETE_REQUEST" });
  assert.equal(deleted.desktop.phase, "Absent");
  assert.equal(deleted.desktop.deletionPhase, "Deleted");
  assert.equal(deleted.desktop.vmPresent, false);
  assert.equal(deleted.desktop.disk, null);
  assertInvariants(deleted);

  const cleanupFailure = model.reduce(deleted, { type: "DELETE_FAILED" });
  assert.equal(cleanupFailure.desktop.deletionPhase, "Deleted");
  assertInvariants(cleanupFailure);
});
