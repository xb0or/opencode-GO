/**
 * 模型路由管理页面。
 *
 * OpenCode Go / Ollama 是可路由上游；OpenRouter 只提供模型元数据。
 * 页面始终以 upstreams + targets 为准，并保留 upstream / protocol /
 * real_model 作为主上游兼容字段。
 */

import { validateRequired } from "../api.js?v=20260719a";
const { ref, reactive, computed } = Vue;

const UPSTREAM_OPTIONS = [
  { id: "go", label: "OpenCode Go" },
  { id: "ollama", label: "Ollama Cloud" },
];

const PROTOCOLS = ["chat", "messages", "responses"];

function defaultTarget(upstream) {
  return {
    real_model: "",
    protocol: "chat",
    group: upstream,
  };
}

function normalizeUpstreams(model) {
  const raw = Array.isArray(model?.upstreams) && model.upstreams.length
    ? model.upstreams
    : [model?.upstream || "go"];
  const known = new Set(UPSTREAM_OPTIONS.map((item) => item.id));
  const selected = [];
  for (const item of raw) {
    const upstream = String(item || "").trim().toLowerCase();
    if (known.has(upstream) && !selected.includes(upstream)) selected.push(upstream);
  }
  const primary = String(model?.upstream || "go").trim().toLowerCase();
  if (known.has(primary) && !selected.includes(primary)) selected.unshift(primary);
  return selected.length ? selected : ["go"];
}

function targetForModel(model, upstream) {
  const target = model?.targets?.[upstream] || {};
  const isPrimary = (model?.upstream || "go") === upstream;
  return {
    real_model:
      String(target.real_model || (isPrimary ? model?.real_model : "") || model?.id || "").trim(),
    protocol: String(target.protocol || model?.protocol || "chat").trim(),
    group: String(
      target.group ||
        model?.upstream_groups?.[upstream] ||
        (isPrimary ? model?.group : "") ||
        upstream
    ).trim(),
  };
}

function parseTags(raw) {
  return String(raw || "")
    .split(/[，,\s]+/)
    .map((item) => item.trim().toLowerCase())
    .filter((item, index, items) => item && items.indexOf(item) === index);
}

function normalizedPricing(model) {
  const pricing = {};
  const values = {
    prompt: model.prompt_price,
    completion: model.completion_price,
    input_cache_read: model.cache_read_price,
    input_cache_write: model.cache_write_price,
  };
  for (const [key, value] of Object.entries(values)) {
    const normalized = String(value || "").trim();
    if (normalized) pricing[key] = normalized;
  }
  return pricing;
}

function sameValue(left, right) {
  return JSON.stringify(left) === JSON.stringify(right);
}

export function useModels(api, showToast, t, showConfirm) {
  const models = ref([]);
  const showModal = ref(false);
  const editingId = ref("");
  const syncing = ref(false);
  const rebuilding = ref(false);
  const showRebuildModal = ref(false);
  const rebuildConfirmation = ref("");
  const syncResult = ref(null);
  const originalPayload = ref(null);
  const searchQuery = ref("");
  const upstreamFilter = ref("all");
  const statusFilter = ref("all");
  const expandedModels = reactive({});

  const newModel = reactive({
    id: "",
    name: "",
    upstream: "go",
    upstreams: ["go"],
    targets: {
      go: defaultTarget("go"),
      ollama: defaultTarget("ollama"),
    },
    context_len: 0,
    status: 1,
    priority: 0,
    tags_text: "",
    prompt_price: "",
    completion_price: "",
    cache_read_price: "",
    cache_write_price: "",
  });

  const stats = computed(() => {
    const all = models.value || [];
    return {
      total: all.length,
      enabled: all.filter((model) => model.status !== 0).length,
      multi: all.filter((model) => normalizeUpstreams(model).length > 1).length,
      matched: all.filter((model) => !!model.openrouter_id).length,
      customized: all.filter((model) => model.is_customized).length,
    };
  });

  const filteredModels = computed(() => {
    const query = searchQuery.value.trim().toLowerCase();
    return (models.value || []).filter((model) => {
      if (statusFilter.value === "enabled" && model.status === 0) return false;
      if (statusFilter.value === "disabled" && model.status !== 0) return false;
      if (
        upstreamFilter.value !== "all" &&
        !normalizeUpstreams(model).includes(upstreamFilter.value)
      ) {
        return false;
      }
      if (!query) return true;
      const routeTargets = normalizeUpstreams(model)
        .map((upstream) => targetForModel(model, upstream).real_model)
        .join(" ");
      return [model.id, model.name, model.openrouter_id, model.openrouter_name, routeTargets]
        .join(" ")
        .toLowerCase()
        .includes(query);
    });
  });

  const canRebuild = computed(
    () => rebuildConfirmation.value.trim().toUpperCase() === "REBUILD"
  );

  function modelUpstreams(model) {
    return normalizeUpstreams(model);
  }

  function modelTarget(model, upstream) {
    return targetForModel(model, upstream);
  }

  function upstreamLabel(upstream) {
    return UPSTREAM_OPTIONS.find((item) => item.id === upstream)?.label || upstream;
  }

  function isPrimaryUpstream(model, upstream) {
    return String(model?.upstream || "go") === upstream;
  }

  function toggleExpanded(id) {
    expandedModels[id] = !expandedModels[id];
  }

  /**
   * 可选目标模型来自当前已同步目录，并按真实上游拆分。
   * input + datalist 仍允许输入目录之外的自定义模型 ID。
   */
  function availableModelsFor(upstream) {
    const byId = new Map();
    for (const model of models.value || []) {
      if (!normalizeUpstreams(model).includes(upstream)) continue;
      const target = targetForModel(model, upstream);
      if (!target.real_model || byId.has(target.real_model)) continue;
      byId.set(target.real_model, {
        id: target.real_model,
        name: model.name || model.id,
        protocol: target.protocol || "chat",
      });
    }
    return Array.from(byId.values()).sort((left, right) => left.id.localeCompare(right.id));
  }

  function applyTargetSuggestion(upstream) {
    const target = newModel.targets[upstream];
    if (!target) return;
    const found = availableModelsFor(upstream).find((item) => item.id === target.real_model);
    if (!found) return;
    target.protocol = found.protocol;
    if (!editingId.value) {
      if (!newModel.id) newModel.id = found.id;
      if (!newModel.name) newModel.name = found.name;
    }
  }

  function toSlug(value) {
    return String(value || "")
      .toLowerCase()
      .replace(/[^a-z0-9\u4e00-\u9fa5]+/g, "-")
      .replace(/^-+|-+$/g, "");
  }

  function fillIDFromName() {
    if (!editingId.value && !newModel.id && newModel.name.trim()) {
      newModel.id = toSlug(newModel.name);
    }
  }

  function resetForm() {
    editingId.value = "";
    originalPayload.value = null;
    newModel.id = "";
    newModel.name = "";
    newModel.upstream = "go";
    newModel.upstreams = ["go"];
    newModel.targets = {
      go: defaultTarget("go"),
      ollama: defaultTarget("ollama"),
    };
    newModel.context_len = 0;
    newModel.status = 1;
    newModel.priority = 0;
    newModel.tags_text = "";
    newModel.prompt_price = "";
    newModel.completion_price = "";
    newModel.cache_read_price = "";
    newModel.cache_write_price = "";
  }

  function formPayload() {
    const upstreams = UPSTREAM_OPTIONS.map((item) => item.id).filter((upstream) =>
      newModel.upstreams.includes(upstream)
    );
    const primary = upstreams.includes(newModel.upstream)
      ? newModel.upstream
      : upstreams[0] || "go";
    const targets = {};
    for (const upstream of upstreams) {
      const source = newModel.targets[upstream] || defaultTarget(upstream);
      targets[upstream] = {
        real_model: String(source.real_model || newModel.id).trim(),
        protocol: PROTOCOLS.includes(source.protocol) ? source.protocol : "chat",
        group: String(source.group || upstream).trim(),
      };
    }
    const primaryTarget = targets[primary] || defaultTarget(primary);
    return {
      id: String(newModel.id || "").trim(),
      name: String(newModel.name || newModel.id || "").trim(),
      upstream: primary,
      upstreams,
      targets,
      protocol: primaryTarget.protocol,
      real_model: primaryTarget.real_model,
      group: primaryTarget.group,
      status: Number(newModel.status) === 0 ? 0 : 1,
      priority: Number(newModel.priority || 0),
      context_len: Math.max(0, Number(newModel.context_len || 0)),
      tags: parseTags(newModel.tags_text),
      pricing: normalizedPricing(newModel),
    };
  }

  function openModal(model) {
    resetForm();
    if (model && model.id) {
      editingId.value = model.id;
      const upstreams = normalizeUpstreams(model);
      const primary = upstreams.includes(model.upstream) ? model.upstream : upstreams[0];
      newModel.id = model.id;
      newModel.name = model.name || model.id;
      newModel.upstream = primary;
      newModel.upstreams = upstreams;
      newModel.targets = {
        go: targetForModel(model, "go"),
        ollama: targetForModel(model, "ollama"),
      };
      newModel.context_len = model.context_len || model.context_length || 0;
      newModel.status = model.status === 0 ? 0 : 1;
      newModel.priority = model.priority || 0;
      newModel.tags_text = (model.tags || []).join(", ");
      newModel.prompt_price = model.pricing?.prompt || "";
      newModel.completion_price = model.pricing?.completion || "";
      newModel.cache_read_price = model.pricing?.input_cache_read || "";
      newModel.cache_write_price = model.pricing?.input_cache_write || "";
      originalPayload.value = formPayload();
    }
    showModal.value = true;
  }

  function openModelSettings(model) {
    openModal(model);
  }

  function closeModal() {
    showModal.value = false;
  }

  function setUpstreamEnabled(upstream, event) {
    const checked = !!event?.target?.checked;
    const selected = new Set(newModel.upstreams);
    if (checked) {
      selected.add(upstream);
    } else {
      if (selected.size === 1 && selected.has(upstream)) {
        if (event?.target) event.target.checked = true;
        showToast(t("models.atLeastOneUpstream"), "error");
        return;
      }
      selected.delete(upstream);
    }
    newModel.upstreams = UPSTREAM_OPTIONS.map((item) => item.id).filter((item) =>
      selected.has(item)
    );
    if (!newModel.upstreams.includes(newModel.upstream)) {
      newModel.upstream = newModel.upstreams[0];
    }
  }

  async function load() {
    try {
      const data = await api("/models", "GET", null, t);
      models.value = data.data || [];
    } catch (error) {
      showToast(error.message, "error");
    }
  }

  function syncToast(result, rebuilt = false) {
    const prefix = rebuilt ? t("models.rebuildDone") : t("models.syncDone");
    const changed = result.updated_count || 0;
    const created = result.created_count || 0;
    const unchanged = result.unchanged_count || 0;
    return `${prefix}: +${created} / ~${changed} / =${unchanged}`;
  }

  async function syncCatalog() {
    if (syncing.value || rebuilding.value) return;
    syncing.value = true;
    try {
      const data = await api("/models/sync", "POST", null, t);
      syncResult.value = { ...data, mode: "sync" };
      showToast(syncToast(data));
      await load();
    } catch (error) {
      showToast(error.message, "error");
    } finally {
      syncing.value = false;
    }
  }

  function openRebuild() {
    rebuildConfirmation.value = "";
    showRebuildModal.value = true;
  }

  function closeRebuild() {
    if (rebuilding.value) return;
    showRebuildModal.value = false;
    rebuildConfirmation.value = "";
  }

  async function rebuildCatalog() {
    if (!canRebuild.value || rebuilding.value || syncing.value) return;
    rebuilding.value = true;
    try {
      const data = await api("/models/rebuild", "POST", { confirmation: "REBUILD" }, t);
      syncResult.value = { ...data, mode: "rebuild" };
      showToast(syncToast(data, true));
      showRebuildModal.value = false;
      rebuildConfirmation.value = "";
      await load();
    } catch (error) {
      showToast(error.message, "error");
    } finally {
      rebuilding.value = false;
    }
  }

  function dismissSyncResult() {
    syncResult.value = null;
  }

  function buildPatchPayload(next) {
    const previous = originalPayload.value || {};
    const patch = {};
    for (const key of ["name", "status", "priority", "context_len"]) {
      if (!sameValue(previous[key], next[key])) patch[key] = next[key];
    }
    if (!sameValue(previous.tags, next.tags)) patch.tags = next.tags;
    if (!sameValue(previous.pricing, next.pricing)) patch.pricing = next.pricing;
    const primaryChanged = !sameValue(previous.upstream, next.upstream);
    const upstreamsChanged = !sameValue(previous.upstreams, next.upstreams);
    if (primaryChanged) patch.upstream = next.upstream;
    if (upstreamsChanged) patch.upstreams = next.upstreams;
    // A legacy multi-upstream row may not yet have Targets. Whenever the
    // primary or membership changes, submit the complete normalized map so
    // each failover provider keeps its own key-pool group.
    if (primaryChanged || upstreamsChanged || !sameValue(previous.targets, next.targets)) {
      patch.targets = next.targets;
    }
    if (!sameValue(previous.protocol, next.protocol)) patch.protocol = next.protocol;
    if (!sameValue(previous.real_model, next.real_model)) patch.real_model = next.real_model;
    if (!sameValue(previous.group, next.group)) patch.group = next.group;
    return patch;
  }

  function validateModel(payload) {
    if (!validateRequired(payload.id, t("models.modelId"), t, showToast)) return false;
    if (!payload.upstreams.length) {
      showToast(t("models.atLeastOneUpstream"), "error");
      return false;
    }
    for (const upstream of payload.upstreams) {
      const target = payload.targets[upstream];
      if (!validateRequired(target.real_model, t("models.upstreamModel"), t, showToast)) {
        return false;
      }
      if (!PROTOCOLS.includes(target.protocol)) {
        showToast(t("models.invalidProtocol"), "error");
        return false;
      }
      if (!validateRequired(target.group, t("models.keyGroup"), t, showToast)) return false;
    }
    return true;
  }

  async function add() {
    const next = formPayload();
    if (!validateModel(next)) return;
    try {
      if (editingId.value) {
        const patch = buildPatchPayload(next);
        if (!Object.keys(patch).length) {
          showToast(t("models.noChanges"));
          closeModal();
          return;
        }
        await api("/models/" + encodeURIComponent(editingId.value), "PATCH", patch, t);
        showToast(t("models.updateBtn") + " ✓");
      } else {
        const payload = { ...next };
        if (!payload.context_len) delete payload.context_len;
        if (!payload.tags.length) delete payload.tags;
        if (!Object.keys(payload.pricing).length) delete payload.pricing;
        await api("/models", "POST", payload, t);
        showToast(t("models.addBtn") + " ✓");
      }
      closeModal();
      await load();
    } catch (error) {
      showToast(error.message, "error");
    }
  }

  async function toggle(id) {
    try {
      await api("/models/" + encodeURIComponent(id) + "/toggle", "POST", null, t);
      await load();
    } catch (error) {
      showToast(error.message, "error");
      await load();
    }
  }

  async function remove(id) {
    const item = models.value.find((model) => model.id === id);
    const name = item ? item.name || item.id : "";
    showConfirm(
      "deleteModel",
      async () => {
        try {
          await api("/models/" + encodeURIComponent(id), "DELETE", null, t);
          showToast(t("models.delete") + " ✓");
          await load();
        } catch (error) {
          showToast(error.message, "error");
        }
      },
      name
    );
  }

  return {
    models,
    filteredModels,
    stats,
    newModel,
    showModal,
    editingId,
    syncing,
    rebuilding,
    showRebuildModal,
    rebuildConfirmation,
    canRebuild,
    syncResult,
    searchQuery,
    upstreamFilter,
    statusFilter,
    expandedModels,
    modelUpstreams,
    modelTarget,
    upstreamLabel,
    isPrimaryUpstream,
    toggleExpanded,
    availableModelsFor,
    applyTargetSuggestion,
    fillIDFromName,
    setUpstreamEnabled,
    openModal,
    openModelSettings,
    closeModal,
    load,
    syncCatalog,
    openRebuild,
    closeRebuild,
    rebuildCatalog,
    dismissSyncResult,
    add,
    toggle,
    remove,
  };
}
