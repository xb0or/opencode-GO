/**
 * API 客户端模块
 *
 * 用法:
 *   const { api, headers } = createApi(tokenRef);
 *   const data = await api("/keys", "GET", null, t);
 */

// Format API errors into user-friendly messages
function fmtApiError(err) {
  if (err instanceof TypeError && err.message === "Failed to fetch") {
    return "网络连接失败，请检查服务是否运行";
  }
  return err.message || "未知错误";
}

// Simple required field check
function validateRequired(value, label, t, showToast) {
  if (!value || (typeof value === "string" && !value.trim())) {
    showToast(label + " " + t("errors.required"), "error");
    return false;
  }
  return true;
}

// Format timestamp for display
function fmtTime(t, locale) {
  if (!t) return "—";
  const d = new Date(t);
  return d.toLocaleString(locale === "zh" ? "zh-CN" : "en-US");
}

/**
 * Create an API client bound to a token ref.
 * @param {import('vue').Ref<string>} tokenRef - Vue ref holding the bearer token
 * @returns {{ api, headers, fmtTime, validateRequired }}
 */
function createApi(tokenRef) {
  function headers() {
    return {
      "Content-Type": "application/json",
      Authorization: "Bearer " + tokenRef.value,
    };
  }

  async function api(path, method = "GET", body = null, t) {
    let r;
    try {
      const opts = { method, headers: headers() };
      if (body) opts.body = JSON.stringify(body);
      r = await fetch("/admin" + path, opts);
    } catch (e) {
      // Network error (DNS, connection refused, timeout, etc.)
      throw new Error(fmtApiError(e));
    }
    if (r.status === 401) {
      tokenRef.value = "";
      localStorage.removeItem("admin_token");
      throw new Error(t ? t("errors.unauthorized") : "Session expired");
    }
    const data = await r.json();
    if (!r.ok)
      throw new Error(data.error || (t ? t("errors.requestFailed") : "Request failed"));
    return data;
  }

  return { api, headers, fmtTime, validateRequired };
}

export { createApi, fmtApiError, validateRequired, fmtTime };
