import { useEffect, useState } from 'react'
import type { ModelConfig, ModelConfigInput, ModelProtocol } from '../api/types'
import { useModels } from '../state/modelsStore'

interface ModelManagerProps {
  open: boolean
  onClose: () => void
}

type Draft = ModelConfigInput & { id?: string }

function emptyDraft(): Draft {
  return {
    name: '',
    protocol: 'openai',
    provider: '',
    base_url: '',
    model: '',
    system_prompt: '',
    temperature: null,
    max_tokens: 0,
    api_key: '',
    is_default: false,
  }
}

function draftFrom(cfg: ModelConfig): Draft {
  return {
    id: cfg.id,
    name: cfg.name,
    protocol: cfg.protocol,
    provider: cfg.provider,
    base_url: cfg.base_url,
    model: cfg.model,
    system_prompt: cfg.system_prompt,
    temperature: cfg.temperature ?? null,
    max_tokens: cfg.max_tokens,
    // 编辑时留空表示「不改动已有密钥」。
    api_key: '',
    is_default: cfg.is_default,
  }
}

/** 模型配置管理弹窗：列表 + 表单 + 连通性测试。 */
export function ModelManager({ open, onClose }: ModelManagerProps) {
  const {
    models,
    loading,
    unsupported,
    error,
    refresh,
    createModel,
    updateModel,
    deleteModel,
    setDefault,
    testModel,
    dismissError,
  } = useModels()

  const [draft, setDraft] = useState<Draft | null>(null)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const [testResult, setTestResult] = useState<Record<string, string>>({})
  const [testing, setTesting] = useState<string | null>(null)

  // 关闭时清空表单，避免下次带着上次的残留。
  useEffect(() => {
    if (!open) {
      setDraft(null)
      setFormError(null)
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open) return null

  const submit = async () => {
    if (!draft) return
    setSaving(true)
    setFormError(null)
    try {
      if (draft.id) {
        await updateModel(draft.id, draft)
      } else {
        await createModel(draft)
      }
      setDraft(null)
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  const runTest = async (id: string) => {
    setTesting(id)
    try {
      const res = await testModel(id)
      setTestResult((prev) => ({ ...prev, [id]: res.message }))
    } catch (err) {
      setTestResult((prev) => ({
        ...prev,
        [id]: err instanceof Error ? err.message : String(err),
      }))
    } finally {
      setTesting(null)
    }
  }

  const remove = async (cfg: ModelConfig) => {
    if (!window.confirm(`删除模型配置「${cfg.name}」？使用它的会话会回退到默认模型。`)) return
    try {
      await deleteModel(cfg.id)
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err))
    }
  }

  return (
    <div className="modal-scrim" onMouseDown={onClose}>
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-label="模型配置"
        onMouseDown={(e) => e.stopPropagation()}
        data-no-window-drag
      >
        <header className="modal-head">
          <h2 className="modal-title">模型配置</h2>
          <button className="icon-btn" type="button" onClick={onClose} title="关闭">
            ✕
          </button>
        </header>

        <div className="modal-body">
          {unsupported && (
            <div className="notice notice-warn">
              当前后端以内存存储运行，不支持保存模型配置。请用 <code>STORE_DRIVER=sqlite</code> 启动。
            </div>
          )}

          {error && (
            <div className="notice notice-error">
              <span>{error}</span>
              <button className="banner-btn" type="button" onClick={dismissError}>
                知道了
              </button>
            </div>
          )}

          {!draft && (
            <>
              <div className="model-list">
                {loading && <div className="model-empty">加载中…</div>}
                {!loading && models.length === 0 && (
                  <div className="model-empty">还没有模型配置，点下方「新增配置」开始。</div>
                )}

                {models.map((m) => (
                  <div className="model-row" key={m.id}>
                    <div className="model-row-main">
                      <div className="model-row-title">
                        <span className="model-row-name">{m.name}</span>
                        {m.is_default && <span className="model-badge">默认</span>}
                        {!m.has_api_key && m.protocol !== 'mock' && (
                          <span className="model-badge is-warn">未设密钥</span>
                        )}
                      </div>
                      <div className="model-row-sub">
                        {m.protocol} · {m.model || '—'}
                        {m.base_url ? ` · ${m.base_url}` : ''}
                      </div>
                      {testResult[m.id] && (
                        <div className="model-row-test">{testResult[m.id]}</div>
                      )}
                    </div>

                    <div className="model-row-actions">
                      <button
                        className="btn-ghost"
                        type="button"
                        disabled={testing === m.id}
                        onClick={() => void runTest(m.id)}
                      >
                        {testing === m.id ? '测试中…' : '测试'}
                      </button>
                      {!m.is_default && (
                        <button
                          className="btn-ghost"
                          type="button"
                          onClick={() => void setDefault(m.id)}
                        >
                          设为默认
                        </button>
                      )}
                      <button className="btn-ghost" type="button" onClick={() => setDraft(draftFrom(m))}>
                        编辑
                      </button>
                      <button className="btn-ghost is-danger" type="button" onClick={() => void remove(m)}>
                        删除
                      </button>
                    </div>
                  </div>
                ))}
              </div>

              <div className="modal-actions">
                <button className="btn-ghost" type="button" onClick={() => void refresh()}>
                  刷新
                </button>
                <button
                  className="btn-primary"
                  type="button"
                  onClick={() => {
                    setFormError(null)
                    setDraft(emptyDraft())
                  }}
                >
                  新增配置
                </button>
              </div>
            </>
          )}

          {draft && (
            <div className="model-form">
              <div className="field">
                <label className="field-label">名称</label>
                <input
                  className="text-input"
                  value={draft.name}
                  placeholder="例如：工作用 GPT-4o"
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                />
                <div className="field-hint">用于在选择器里区分不同配置</div>
              </div>

              <div className="field-row">
                <div className="field">
                  <label className="field-label">协议</label>
                  <select
                    className="text-input"
                    value={draft.protocol}
                    onChange={(e) =>
                      setDraft({ ...draft, protocol: e.target.value as ModelProtocol })
                    }
                  >
                    <option value="openai">OpenAI 兼容（/chat/completions）</option>
                    <option value="anthropic">Anthropic（/messages）</option>
                    {/* 兼容历史数据：早期可能建过 mock 配置，编辑时保留其原值，
                        否则 select 找不到匹配项会静默显示成第一项，造成"看起来是
                        openai、实际存的是 mock"的错配。 */}
                    {draft.protocol === 'mock' && (
                      <option value="mock">Mock（历史配置，建议改用上方两种协议）</option>
                    )}
                  </select>
                </div>

                <div className="field">
                  <label className="field-label">模型名</label>
                  <input
                    className="text-input"
                    value={draft.model ?? ''}
                    placeholder={draft.protocol === 'anthropic' ? 'claude-sonnet-4-20250514' : 'gpt-4o-mini'}
                    onChange={(e) => setDraft({ ...draft, model: e.target.value })}
                  />
                </div>
              </div>

              <div className="field">
                <label className="field-label">接口地址</label>
                <input
                  className="text-input"
                  value={draft.base_url ?? ''}
                  placeholder={
                    draft.protocol === 'anthropic'
                      ? 'https://api.anthropic.com/v1'
                      : 'https://api.openai.com/v1'
                  }
                  onChange={(e) => setDraft({ ...draft, base_url: e.target.value })}
                />
                <div className="field-hint">
                  填到域名或路径前缀即可，版本段（<code>/v1</code>）会自动补全；已包含则不会重复添加
                </div>
              </div>

              <div className="field">
                <label className="field-label">
                  API Key
                  {draft.id && <span className="field-hint-inline">（留空表示不修改）</span>}
                </label>
                <input
                  className="text-input"
                  type="password"
                  autoComplete="off"
                  value={draft.api_key ?? ''}
                  placeholder={draft.id ? '••••••••（已保存）' : 'sk-...'}
                  onChange={(e) => setDraft({ ...draft, api_key: e.target.value })}
                />
                <div className="field-hint">仅保存在本机，加密后写入数据库，接口不会返回明文</div>
              </div>

              <div className="field-row">
                <div className="field">
                  <label className="field-label">Temperature</label>
                  <input
                    className="text-input"
                    type="number"
                    min={0}
                    max={2}
                    step={0.1}
                    value={draft.temperature ?? ''}
                    placeholder="默认"
                    onChange={(e) =>
                      setDraft({
                        ...draft,
                        temperature: e.target.value === '' ? null : Number(e.target.value),
                      })
                    }
                  />
                </div>
                <div className="field">
                  <label className="field-label">Max Tokens</label>
                  <input
                    className="text-input"
                    type="number"
                    min={0}
                    value={draft.max_tokens ?? 0}
                    onChange={(e) =>
                      setDraft({ ...draft, max_tokens: Number(e.target.value) || 0 })
                    }
                  />
                </div>
              </div>

              <div className="field">
                <label className="field-label">系统提示（可选）</label>
                <textarea
                  className="text-input"
                  rows={3}
                  value={draft.system_prompt ?? ''}
                  placeholder="你是一个乐于助人的助手…"
                  onChange={(e) => setDraft({ ...draft, system_prompt: e.target.value })}
                />
              </div>

              <label className="checkbox-row">
                <input
                  type="checkbox"
                  checked={Boolean(draft.is_default)}
                  onChange={(e) => setDraft({ ...draft, is_default: e.target.checked })}
                />
                <span>设为默认模型（新会话默认使用）</span>
              </label>

              {formError && <div className="notice notice-error">{formError}</div>}

              <div className="modal-actions">
                <button className="btn-ghost" type="button" onClick={() => setDraft(null)}>
                  取消
                </button>
                <button
                  className="btn-primary"
                  type="button"
                  disabled={saving || !draft.name.trim()}
                  onClick={() => void submit()}
                >
                  {saving ? '保存中…' : '保存'}
                </button>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
