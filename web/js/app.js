/**
 * OpenCode-SW Admin 管理面板 - 主入口
 *
 * ES Module 入口文件，导入各模块并创建 Vue 3 应用。
 */

import { icons } from "./icons.js?v=20260619a";
import { locales } from "./locales.js?v=20260619a";
import { createApi, fmtTime } from "./api.js?v=20260619a";
import { useDashboard } from "./pages/dashboard.js?v=20260619a";
import { useKeys } from "./pages/keys.js?v=20260619a";
import { useTokens } from "./pages/tokens.js?v=20260619a";
import { useModels } from "./pages/models.js?v=20260619a";
import { useMappings } from "./pages/mappings.js?v=20260619a";
import { useOps } from "./pages/ops.js?v=20260619a";
import { useUsage } from "./pages/usage.js?v=20260619a";

const { createApp, reactive, ref, watch } = Vue;

createApp({
  setup() {
    // ─── 全局状态 ─────────────────────────────────────
    const token = ref(localStorage.getItem("admin_token") || "");
    const password = ref("");
    const loginError = ref("");
    const logging = ref(false);
    const page = ref("dashboard");
    const locale = ref(localStorage.getItem("admin_locale") || "zh");
    const theme = ref(localStorage.getItem("admin_theme") || "dark");
    const dropLang = ref(false);
    const dropTheme = ref(false);

    const toast = reactive({ show: false, msg: "", type: "success" });
    const confirm = reactive({
      show: false,
      title: "",
      msg: "",
      okText: "",
      cancelText: "",
      danger: false,
      onOk: null,
    });

    function showToast(msg, type = "success") {
      toast.show = true;
      toast.msg = msg;
      toast.type = type;
      const duration = type === "error" ? 5000 : 3000;
      setTimeout(() => (toast.show = false), duration);
    }

    function fmtNumber(n) {
      if (n === null || n === undefined || n === "") return "—";
      const value = Number(n);
      if (!Number.isFinite(value) || value <= 0) return "—";
      return value.toLocaleString();
    }

    function emptyLabel(value) {
      if (value === null || value === undefined) return "—";
      if (typeof value === "string" && value.trim() === "") return "—";
      return value;
    }

    function streamLabel(stream) {
      return stream ? t("table.streamYes") : t("table.streamNo");
    }

    function streamBadgeClass(stream) {
      return "badge " + (stream ? "badge-accent" : "badge-blue");
    }

    function fmtPrice(pricing, key) {
      if (!pricing || !pricing[key]) return "—";
      const perToken = Number(pricing[key]);
      if (!Number.isFinite(perToken)) return pricing[key];
      if (perToken < 0) return "—";
      return "$" + (perToken * 1000000).toFixed(3) + "/1M";
    }

    function fmtCapabilities(model) {
      if (Array.isArray(model.tags) && model.tags.length) {
        return model.tags;
      }
      const modalities = model.architecture?.input_modalities || [];
      const params = model.supported_parameters || [];
      const caps = [];
      if (!modalities.length || modalities.includes("text")) caps.push("text");
      if (modalities.includes("image")) caps.push("vision");
      if (modalities.includes("video")) caps.push("video");
      if (params.includes("tools")) caps.push("tools");
      if (params.includes("structured_outputs") || params.includes("response_format"))
        caps.push("structured");
      if (params.includes("reasoning") || params.includes("include_reasoning"))
        caps.push("reasoning");
      return caps.length ? caps : ["text"];
    }

    function capabilityLabel(capability) {
      return t("capabilities." + capability);
    }

    async function copyText(text) {
      try {
        await navigator.clipboard.writeText(text);
        showToast(t("common.copied") + " ✓");
      } catch (e) {
        showToast(t("common.copyFailed"), "error");
      }
    }

    // ─── 国际化 ───────────────────────────────────────
    function t(key, params) {
      const keys = key.split(".");
      let val = locales[locale.value];
      for (const k of keys) if (val) val = val[k];
      if (typeof val === "string" && params)
        for (const [k, v] of Object.entries(params))
          val = val.replace("{" + k + "}", v);
      return val || key;
    }

    watch(locale, (val) => {
      localStorage.setItem("admin_locale", val);
      document.documentElement.lang = val === "zh" ? "zh-CN" : "en";
    });

    // ─── 主题 ─────────────────────────────────────────
    function toggleTheme() {
      theme.value = theme.value === "dark" ? "light" : "dark";
    }
    // Apply theme on init
    document.documentElement.dataset.theme = theme.value;
    watch(theme, (val) => {
      localStorage.setItem("admin_theme", val);
      document.documentElement.dataset.theme = val;
    });

    // ─── API 客户端 ───────────────────────────────────
    const { api } = createApi(token);

    // ─── 登录 / 注销 ──────────────────────────────────
    async function login() {
      loginError.value = "";
      logging.value = true;
      try {
        const d = await api("/login", "POST", { password: password.value });
        token.value = d.token;
        localStorage.setItem("admin_token", d.token);
        password.value = "";
        logging.value = false;
        dashboard.load();
      } catch (e) {
        logging.value = false;
        loginError.value = e.message;
      }
    }

    function logout() {
      token.value = "";
      localStorage.removeItem("admin_token");
    }

    // ─── 确认弹窗 ─────────────────────────────────────
    function showConfirm(type, item, name) {
      confirm.cancelText = t("confirm.cancel");
      if (type === "logout") {
        confirm.title = t("confirm.logout.title");
        confirm.msg = t("confirm.logout.msg");
        confirm.okText = t("confirm.logout.ok");
        confirm.danger = false;
        confirm.onOk = () => logout();
      } else if (type === "deleteKey") {
        confirm.title = t("confirm.deleteKey.title");
        confirm.msg = t("confirm.deleteKey.msg", { name: name || "" });
        confirm.okText = t("confirm.deleteKey.ok");
        confirm.danger = true;
        confirm.onOk = item;
      } else if (type === "deleteToken") {
        confirm.title = t("confirm.deleteToken.title");
        confirm.msg = t("confirm.deleteToken.msg", { name: name || "" });
        confirm.okText = t("confirm.deleteToken.ok");
        confirm.danger = true;
        confirm.onOk = item;
      } else if (type === "deleteModel") {
        confirm.title = t("confirm.deleteModel.title");
        confirm.msg = t("confirm.deleteModel.msg", { name: name || "" });
        confirm.okText = t("confirm.deleteModel.ok");
        confirm.danger = true;
        confirm.onOk = item;
      } else if (type === "deleteMapping") {
        confirm.title = t("confirm.deleteMapping.title");
        confirm.msg = t("confirm.deleteMapping.msg", { name: name || "" });
        confirm.okText = t("confirm.deleteMapping.ok");
        confirm.danger = true;
        confirm.onOk = item;
      }
      confirm.show = true;
    }
    function confirmCancel() {
      confirm.show = false;
    }
    async function confirmOk() {
      const fn = confirm.onOk;
      confirm.show = false;
      if (fn) await fn();
    }

    // ─── 页面组合式函数 ───────────────────────────────
    const dashboard = useDashboard(api, showToast, t);
    const ops = useOps(api, showToast, t);
    const usage = useUsage(api, showToast, t);
    const keys = useKeys(api, showToast, t, showConfirm);
    const tokens = useTokens(api, showToast, t, showConfirm);
    const models = useModels(api, showToast, t, showConfirm);
    const mappings = useMappings(api, showToast, t, showConfirm);

    // ─── 初始化 ───────────────────────────────────────
    if (token.value) dashboard.load();

    function openPage(nextPage) {
      page.value = nextPage;
      if (nextPage === "dashboard") dashboard.load();
      else if (nextPage === "ops") ops.load();
      else if (nextPage === "usage") usage.load();
      else if (nextPage === "keys") keys.load();
      else if (nextPage === "tokens") tokens.load();
      else if (nextPage === "models") models.load();
      else if (nextPage === "mappings") mappings.load();
    }

    // ─── 暴露给模板 ───────────────────────────────────
    return {
      // 全局
      token,
      password,
      loginError,
      logging,
      page,
      locale,
      theme,
      dropLang,
      dropTheme,
      toast,
      confirm,
      icons,
      t,
      login,
      logout,
      openPage,
      toggleTheme,
      showConfirm,
      confirmCancel,
      confirmOk,
      showToast,
      fmtTime,
      fmtNumber,
      emptyLabel,
      streamLabel,
      streamBadgeClass,
      fmtPrice,
      fmtCapabilities,
      capabilityLabel,
      copyText,

      // 仪表盘
      stats: dashboard.stats,
      compactNumber: dashboard.compactNumber,
      formatTokens: dashboard.formatTokens,
      formatCost: dashboard.formatCost,
      formatPercent: dashboard.formatPercent,
      formatDuration: dashboard.formatDuration,
      healthScore: dashboard.healthScore,
      statusLabel: dashboard.statusLabel,
      recentErrors: dashboard.recentErrors,

      // 运维监控
      opsStats: ops.stats,
      opsHealth: ops.health,
      opsLoading: ops.loading,
      loadOps: ops.load,
      opsPercent: ops.formatPercent,
      opsDuration: ops.formatDuration,
      opsRate: ops.formatRate,
      opsPool: ops.pool,
      opsHealthScore: ops.healthScore,
      opsRecentErrors: ops.recentErrors,
      opsUpdatedLabel: ops.updatedLabel,

      // 使用记录
      usageLogs: usage.logs,
      usageLoading: usage.loading,
      usageFilters: usage.filters,
      usagePagination: usage.pagination,
      usageSummary: usage.summary,
      usageSelected: usage.selected,
      loadUsage: usage.load,
      applyUsageFilters: usage.apply,
      resetUsageFilters: usage.reset,
      setTodayUsageFilters: usage.setToday,
      usageNextPage: usage.nextPage,
      usagePrevPage: usage.prevPage,
      openUsageDetail: usage.openDetail,
      closeUsageDetail: usage.closeDetail,
      usageDuration: usage.formatDuration,
      usageTokens: usage.formatTokens,
      usageCost: usage.formatCost,
      usageUnitPrice: usage.formatUnitPrice,
      usagePriceBrief: usage.priceBrief,
      usageGroupLabel: usage.groupLabel,
      usageTokenLabel: usage.tokenLabel,
      usageModelLabel: usage.modelLabel,
      usageModelInitial: usage.modelInitial,
      usageStreamRate: usage.streamRate,
      usageLatencyLine: usage.latencyLine,
      usageCacheReadTokens: usage.cacheReadTokens,
      usageCacheCreationTokens: usage.cacheCreationTokens,
      usageFinalCost: usage.finalCost,
      usageBillingMode: usage.billingMode,
      usagePageRange: usage.pageRange,

      // 密钥管理
      keys: keys.keys,
      newKey: keys.newKey,
      editingKeyId: keys.editingKeyId,
      showKeyModal: keys.showKeyModal,
      quotaLoading: keys.quotaLoading,
      quotaData: keys.quotaData,
      openKeyModal: keys.openKeyModal,
      openKeySettings: keys.openKeySettings,
      closeKeyModal: keys.closeKeyModal,
      loadKeys: keys.load,
      addKey: keys.add,
      toggleKey: keys.toggle,
      resetCooldown: keys.resetCooldown,
      fetchQuota: keys.fetchQuota,
      useQuotaWorkspaceCandidate: keys.useQuotaWorkspaceCandidate,
      deleteKey: keys.remove,
      quotaPercent: keys.quotaPercent,
      quotaBadgeClass: keys.quotaBadgeClass,
      quotaWorkspaceCandidates: keys.quotaWorkspaceCandidates,
      quotaCandidateLabel: keys.quotaCandidateLabel,
      normalizeKeyCookie: keys.normalizeKeyCookie,

      // 令牌管理
      tokens: tokens.tokens,
      newToken: tokens.newToken,
      editingTokenId: tokens.editingTokenId,
      showTokenModal: tokens.showTokenModal,
      openTokenModal: tokens.openTokenModal,
      openTokenSettings: tokens.openTokenSettings,
      closeTokenModal: tokens.closeTokenModal,
      loadTokens: tokens.load,
      addToken: tokens.add,
      toggleToken: tokens.toggle,
      deleteToken: tokens.remove,
      fmtTokenExpiry: tokens.fmtTokenExpiry,
      expiryBadgeClass: tokens.expiryBadgeClass,
      requestUsedLabel: tokens.requestUsedLabel,
      requestUsedPercent: tokens.requestUsedPercent,
      requestBadgeClass: tokens.requestBadgeClass,

      // 模型管理
      models: models.models,
      newModel: models.newModel,
      showModal: models.showModal,
      editingModelId: models.editingId,
      syncingModels: models.syncing,
      availableModels: models.availableModels,
      openModal: models.openModal,
      openModelSettings: models.openModelSettings,
      closeModal: models.closeModal,
      loadModels: models.load,
      syncModels: models.syncCatalog,
      addModel: models.add,
      toggleModel: models.toggle,
      deleteModel: models.remove,

      // 模型映射管理
      mappings: mappings.mappings,
      newMapping: mappings.newMapping,
      editingSource: mappings.editingSource,
      showMappingModal: mappings.showMappingModal,
      openMappingModal: mappings.openMappingModal,
      openMappingSettings: mappings.openMappingSettings,
      closeMappingModal: mappings.closeMappingModal,
      loadMappings: mappings.load,
      addMapping: mappings.add,
      deleteMapping: mappings.remove,
    };
  },
}).mount("#app");
