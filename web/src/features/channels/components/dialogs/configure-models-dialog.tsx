/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { PluginIcon } from '@/features/task-plugins/components/plugin-icon'

import type { TaskPluginOption } from '../../api'
import {
  openRouterBaseModel,
  resolveOpenRouterModel,
  openRouterModelRoutingSchema,
  type OpenRouterModelRouting,
} from '../../lib/openrouter-routing'
import {
  ModelMappingEditor,
  type ModelMappingDraftRequest,
} from '../model-mapping-editor'
import {
  OpenRouterModelRoutingEditor,
  OpenRouterRoutingBadge,
  type OpenRouterConnection,
} from '../openrouter-model-routing-editor'
import { UpstreamModelSelection } from '../upstream-model-selection'

export type OpenRouterModelsDraft = {
  modelMapping: string
  routing: OpenRouterModelRouting
}

type ConfigureModelsDialogProps = {
  open: boolean
  models: string[]
  plugins?: TaskPluginOption[]
  initialPluginKey?: string
  onOpenChange: (open: boolean) => void
  onApply: (models: string[], openRouter?: OpenRouterModelsDraft) => void
  disabled?: boolean
  initialModel?: string
  openRouter?: OpenRouterModelsDraft & {
    connection: OpenRouterConnection
    enterprise?: boolean
    readOnly?: boolean
  }
  /** Called after the selection is applied when the user starts a redirect for a model. */
  onRedirect?: (model: string) => void
}

export function ConfigureModelsDialog(props: ConfigureModelsDialogProps) {
  const { t } = useTranslation()
  // Mounted for each opening. Keep unchecked candidates available until close.
  const [initialModels] = useState(props.models)
  const [selected, setSelected] = useState(initialModels)
  const [mapping, setMapping] = useState(props.openRouter?.modelMapping ?? '')
  const [routing, setRouting] = useState(props.openRouter?.routing ?? {})
  const routingByModel = useMemo(
    () => new Map(Object.entries(routing)),
    [routing]
  )
  const [activeModel, setActiveModel] = useState(props.initialModel ?? '')
  const [showMappings, setShowMappings] = useState(false)
  const [mappingValid, setMappingValid] = useState(true)
  const [mappingDraft, setMappingDraft] =
    useState<ModelMappingDraftRequest | null>(null)
  const routingButton = useRef<HTMLButtonElement | null>(null)
  const mapped = useMemo(() => {
    try {
      const value: unknown = JSON.parse(mapping || '{}')
      if (
        !value ||
        typeof value !== 'object' ||
        Array.isArray(value) ||
        Object.values(value).some(
          (target) => typeof target !== 'string' || !target.trim()
        )
      ) {
        return null
      }
      return Object.assign(Object.create(null), value) as Record<string, string>
    } catch {
      return null
    }
  }, [mapping])
  const resolvedModels = useMemo(
    () =>
      new Map(
        selected.map((model) => [
          model,
          mapped ? resolveOpenRouterModel(model, mapped) : null,
        ])
      ),
    [selected, mapped]
  )
  const hasMappingCycle =
    mapped !== null && [...resolvedModels.values()].includes(null)
  const activeUpstream = resolvedModels.get(activeModel) ?? null
  const initialMapping = useRef(props.openRouter?.modelMapping ?? '')
  const removedModels = new Set(
    initialModels.filter((model) => !selected.includes(model))
  )
  const retainedRouting = Object.fromEntries(
    Object.entries(routing).filter(([model]) => !removedModels.has(model))
  )
  const routingValid =
    openRouterModelRoutingSchema.safeParse(retainedRouting).success
  const routingDisabled =
    props.disabled || props.openRouter?.enterprise || props.openRouter?.readOnly
  const [source, setSource] = useState(
    props.initialPluginKey ? `plugin:${props.initialPluginKey}` : 'all'
  )
  const plugins =
    props.plugins?.filter((plugin) => plugin.models.length > 0) ?? []
  const plugin = plugins.find((item) => `plugin:${item.key}` === source)
  const models = [
    ...new Set(
      plugin
        ? plugin.models
        : [
            ...initialModels,
            ...selected,
            ...plugins.flatMap((item) => item.models),
          ]
    ),
  ]
  const onRedirect = props.openRouter ? undefined : props.onRedirect
  // Redirecting keeps the current picks and makes sure the redirected model is
  // published, otherwise the alias could never be called.
  const handleRedirect = onRedirect
    ? (model: string) => {
        props.onApply(
          selected.includes(model) ? selected : [...selected, model]
        )
        props.onOpenChange(false)
        onRedirect(model)
      }
    : undefined
  const commitMappings = (value: string) => {
    try {
      const previous = JSON.parse(initialMapping.current || '{}') as Record<
        string,
        string
      >
      const next = JSON.parse(value || '{}') as Record<string, unknown>
      const added = Object.entries(next)
        .filter(
          ([name, target]) =>
            !Object.hasOwn(previous, name) &&
            typeof target === 'string' &&
            target.trim()
        )
        .map(([name]) => name)
      setSelected((models) => [...new Set([...models, ...added])])
    } catch {
      /* The shared editor displays invalid mappings. */
    }
  }
  const apply = () => {
    if (props.disabled) return
    if (!props.openRouter || props.openRouter.readOnly) {
      props.onApply(selected)
      props.onOpenChange(false)
      return
    }
    if (!mapped || !mappingValid || !routingValid || hasMappingCycle) return
    // Remove policies only for models deliberately unchecked in this dialog.
    props.onApply(selected, { modelMapping: mapping, routing: retainedRouting })
    props.onOpenChange(false)
  }
  const selection = (
    <UpstreamModelSelection
      key={plugin?.key ?? 'all'}
      models={models}
      selected={selected}
      existingModels={initialModels}
      onChange={(models) => {
        setSelected(models)
        if (activeModel && !models.includes(activeModel)) setActiveModel('')
      }}
      disabled={props.disabled}
      renderModelAction={
        props.openRouter
          ? (model) =>
              selected.includes(model) && (
                <div className='ml-auto flex min-w-0 flex-wrap items-center justify-end gap-1'>
                  <OpenRouterRoutingBadge
                    policy={routingByModel.get(model)}
                    upstream={resolvedModels.get(model) ?? null}
                  />
                  <Button
                    type='button'
                    variant='ghost'
                    size='sm'
                    disabled={
                      Boolean(routingDisabled) ||
                      !mapped ||
                      resolvedModels.get(model) === null
                    }
                    aria-label={t('Configure routing for {{model}}', { model })}
                    aria-expanded={activeModel === model}
                    onClick={(event) => {
                      routingButton.current = event.currentTarget
                      setActiveModel(activeModel === model ? '' : model)
                    }}
                  >
                    {t('Routing')}
                  </Button>
                </div>
              )
          : undefined
      }
      onRedirectModel={handleRedirect}
      showChanges={false}
      summaryText={
        plugins.length > 0
          ? t('Selected {{selected}} / {{total}}', {
              selected: models.filter((model) => selected.includes(model))
                .length,
              total: models.length,
            })
          : t('Current models: {{count}}', { count: models.length })
      }
    />
  )

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Configure Models')}
      description={
        plugins.length > 0
          ? t('Select models and apply to channel models list.')
          : t('Select the models to keep in this channel.')
      }
      contentClassName='sm:max-w-3xl'
      bodyClassName='space-y-4'
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            onClick={() => props.onOpenChange(false)}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='button'
            disabled={
              props.disabled ||
              Boolean(
                props.openRouter &&
                !props.openRouter.readOnly &&
                (!mapped || !mappingValid || !routingValid || hasMappingCycle)
              )
            }
            onClick={apply}
          >
            {t('Apply')}
          </Button>
        </>
      }
    >
      {props.openRouter && hasMappingCycle && (
        <Alert variant='destructive'>
          <AlertDescription>
            {t(
              'Model mapping contains a cycle. Fix the mapping before configuring routing.'
            )}
          </AlertDescription>
        </Alert>
      )}
      {props.openRouter?.enterprise && (
        <Alert>
          <AlertDescription>
            {t('Per-model routing is unavailable for enterprise accounts.')}
          </AlertDescription>
        </Alert>
      )}
      {plugins.length > 0 ? (
        <Tabs
          value={plugin ? `plugin:${plugin.key}` : 'all'}
          onValueChange={(value) => setSource(String(value))}
          className='min-w-0 gap-3'
        >
          <div className='max-w-full overflow-x-auto'>
            <TabsList
              variant='line'
              aria-label={t('Models')}
              className='min-w-max'
            >
              <TabsTrigger value='all'>{t('All')}</TabsTrigger>
              {plugins.map((item) => (
                <TabsTrigger
                  key={item.key}
                  value={`plugin:${item.key}`}
                  title={item.name}
                >
                  <span aria-hidden='true'>
                    <PluginIcon plugin={item} size={16} />
                  </span>
                  <span className='max-w-40 truncate'>{item.name}</span>
                </TabsTrigger>
              ))}
            </TabsList>
          </div>
          <TabsContent value={plugin ? `plugin:${plugin.key}` : 'all'}>
            {selection}
          </TabsContent>
        </Tabs>
      ) : (
        selection
      )}
      {props.openRouter &&
        activeModel &&
        activeUpstream !== null &&
        selected.includes(activeModel) && (
          <OpenRouterModelRoutingEditor
            model={activeModel}
            upstream={activeUpstream}
            policy={routingByModel.get(activeModel)}
            connection={props.openRouter.connection}
            disabled={Boolean(routingDisabled)}
            onClose={() => {
              setActiveModel('')
              routingButton.current?.focus()
            }}
            onChange={(policy) => {
              if (routingDisabled || !mapped) return
              setRouting({ ...routing, [activeModel]: policy })
              const upstream = activeUpstream
              const base = openRouterBaseModel(upstream)
              if (base !== upstream) {
                setMapping(
                  JSON.stringify({ ...mapped, [activeModel]: base }, null, 2)
                )
              }
            }}
            onAddAlias={() => {
              if (routingDisabled) return
              setShowMappings(true)
              setMappingValid(false)
              setMappingDraft({
                from: '',
                to: openRouterBaseModel(activeUpstream),
                token: Date.now(),
              })
            }}
          />
        )}
      {props.openRouter && showMappings && (
        <section
          className='space-y-3 rounded-lg border p-3'
          aria-label={t('Model Mapping')}
        >
          <p className='text-muted-foreground text-sm'>
            {t(
              'Add an alias, then configure its routing in the model list. Changes apply together.'
            )}
          </p>
          <ModelMappingEditor
            value={mapping}
            onChange={setMapping}
            disabled={Boolean(routingDisabled)}
            sourceModelOptions={selected}
            targetModelOptions={[...new Set(Object.values(mapped ?? {}))]}
            draftRequest={mappingDraft}
            onDraftRequestHandled={() => setMappingDraft(null)}
            onValidityChange={setMappingValid}
            onCommit={commitMappings}
          />
          <Button
            type='button'
            variant='outline'
            size='sm'
            disabled={Boolean(routingDisabled) || !mapped || !mappingValid}
            onClick={() => {
              commitMappings(mapping)
              setShowMappings(false)
            }}
          >
            {t('Done')}
          </Button>
        </section>
      )}
      {props.openRouter &&
        !props.openRouter.readOnly &&
        (!mapped || !mappingValid || !routingValid || hasMappingCycle) && (
          <p role='alert' className='text-destructive text-sm'>
            {t(
              'Complete the model mapping and routing fields before applying.'
            )}
          </p>
        )}
    </Dialog>
  )
}
