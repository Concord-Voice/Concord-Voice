import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type MutableRefObject,
  type ReactNode,
} from 'react';
import { useClientConfigStore } from '../../stores/clientConfigStore';
import { clientConfigService } from '../../services/clientConfigService';
import {
  getPresenceHistoryPage,
  getPresenceHistorySettings,
  PresenceHistoryRequestError,
  type PresenceHistoryItem,
  type PresenceHistorySettings,
} from '../../services/presenceHistoryService';
import './PresenceHistorySection.css';

export interface PresenceHistorySectionProps {
  readonly userId: string | null;
}

type LoadMoreState = 'idle' | 'loading' | 'error';
type InitialLoadReason = 'initial' | 'retry';

interface ReadyPresenceHistoryFeedState {
  ownerId: string;
  status: 'ready';
  settings: PresenceHistorySettings;
  items: PresenceHistoryItem[];
  nextCursor: string | null;
  loadMoreState: LoadMoreState;
}

type PresenceHistoryFeedState =
  | { ownerId: string | null; status: 'idle' }
  | { ownerId: string; status: 'loading' }
  | {
      ownerId: string;
      status: 'error';
      confirmed: ReadyPresenceHistoryFeedState | null;
      routeMissing: boolean;
    }
  | ReadyPresenceHistoryFeedState;

interface OwnerMessage {
  ownerId: string | null;
  message: string;
}

interface CategoryPresentation {
  label: string;
  icon: string;
}

const CATEGORY_PRESENTATION: Record<PresenceHistoryItem['category'], CategoryPresentation> = {
  server_voice: { label: 'Server Voice', icon: '◉' },
  private_call: { label: 'Private Call', icon: '☎' },
  games: { label: 'Games', icon: '◆' },
  music: { label: 'Music', icon: '♪' },
  streaming: { label: 'Streaming', icon: '▶' },
  browser: { label: 'Browser', icon: '◎' },
  productivity: { label: 'Productivity', icon: '✓' },
  creator: { label: 'Creator', icon: '✦' },
  custom_text: { label: 'Custom Status', icon: '●' },
};
const LOAD_MORE_RETRY_ID = 'presence-history-load-more-retry';

const historyDateFormatter = new Intl.DateTimeFormat(undefined, {
  year: 'numeric',
  month: 'short',
  day: 'numeric',
  hour: 'numeric',
  minute: '2-digit',
});

function emptyFeedState(ownerId: string | null): PresenceHistoryFeedState {
  return { ownerId, status: 'idle' };
}

function itemElementId(itemId: string): string {
  return `presence-history-item-${itemId}`;
}

function itemHeadingId(itemId: string): string {
  return `${itemElementId(itemId)}-heading`;
}

function formatTimestamp(value: string): string {
  return historyDateFormatter.format(new Date(value));
}

function formatDuration(startedAt: string, endedAt: string): string {
  const elapsedMilliseconds = Math.max(0, Date.parse(endedAt) - Date.parse(startedAt));
  const totalMinutes = Math.max(1, Math.round(elapsedMilliseconds / 60_000));
  if (totalMinutes < 60) {
    return `${totalMinutes} ${totalMinutes === 1 ? 'minute' : 'minutes'}`;
  }

  const totalHours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (totalHours < 24) {
    const hoursLabel = `${totalHours} ${totalHours === 1 ? 'hour' : 'hours'}`;
    return minutes === 0 ? hoursLabel : `${hoursLabel} ${minutes} minutes`;
  }

  const days = Math.floor(totalHours / 24);
  const hours = totalHours % 24;
  const daysLabel = `${days} ${days === 1 ? 'day' : 'days'}`;
  return hours === 0 ? daysLabel : `${daysLabel} ${hours} hours`;
}

function activityCount(count: number): string {
  return count === 1 ? '1 activity' : `${count} activities`;
}

function initialAnnouncement(count: number, reason: InitialLoadReason): string {
  if (count === 0) {
    return reason === 'retry'
      ? 'Activity History loaded after retry. No records found.'
      : 'Activity History loaded. No records found.';
  }
  const suffix = reason === 'retry' ? ' after retry' : '';
  return `Loaded ${activityCount(count)}${suffix}.`;
}

function loadMoreAnnouncement(count: number, terminal: boolean): string {
  let loaded = `Loaded ${count} more activities.`;
  if (count === 0) loaded = 'No additional activity was found.';
  if (count === 1) loaded = 'Loaded 1 more activity.';
  return terminal ? `${loaded} All history is shown.` : loaded;
}

function isCurrentRequest(
  controller: AbortController,
  generation: number,
  ownerId: string,
  generationRef: MutableRefObject<number>,
  ownerRef: MutableRefObject<string | null>
): boolean {
  return (
    !controller.signal.aborted &&
    generationRef.current === generation &&
    ownerRef.current === ownerId
  );
}

function retryCapabilityAvailability(): void {
  clientConfigService.refreshServerCapabilities().catch(() => undefined);
}

function isMissingHistoryRoute(error: unknown): boolean {
  return (
    error instanceof PresenceHistoryRequestError && (error.status === 404 || error.status === 405)
  );
}

interface CapabilityAvailabilityNoticeProps {
  readonly staleSupported: boolean;
}

function CapabilityAvailabilityNotice({ staleSupported }: CapabilityAvailabilityNoticeProps) {
  if (staleSupported) {
    return (
      <aside
        className="presence-history__state presence-history__state--error presence-history__state--stale"
        aria-label="Activity History availability"
        aria-live="polite"
        aria-atomic="true"
      >
        <p>
          Activity History availability could not be refreshed. Showing the last confirmed supported
          feed.
        </p>
        <button type="button" onClick={retryCapabilityAvailability}>
          Retry availability check
        </button>
      </aside>
    );
  }

  return (
    <div
      className="presence-history__state presence-history__state--error"
      role="alert"
      aria-atomic="true"
    >
      <p>Activity History availability could not be confirmed.</p>
      <button type="button" onClick={retryCapabilityAvailability}>
        Retry availability check
      </button>
    </div>
  );
}

interface EmptyHistoryProps {
  readonly settings: PresenceHistorySettings;
}

function EmptyHistory({ settings }: EmptyHistoryProps) {
  let title = 'Activity History is off';
  let description =
    'Turn it on in Privacy & Security. History begins with your next Custom Status change.';

  if (settings.reconsentRequired) {
    title = 'Recording is paused';
    description =
      'Review the updated terms in Privacy & Security. When recording resumes, history begins again with your next Custom Status change.';
  } else if (settings.enabled) {
    title = 'No activity recorded yet';
    description = 'History begins with your next Custom Status change.';
  }

  return (
    <div className="presence-history__empty">
      <h4>{title}</h4>
      <p>{description}</p>
    </div>
  );
}

interface HistoryItemCardProps {
  readonly item: PresenceHistoryItem;
}

function HistoryItemCard({ item }: HistoryItemCardProps) {
  const presentation = CATEGORY_PRESENTATION[item.category];
  const headingId = itemHeadingId(item.id);
  const endedAt = item.endedAt;
  let payloadContent: ReactNode;
  if (item.status === 'supported') {
    let emoji: ReactNode = null;
    if (item.payload.emoji !== undefined) {
      emoji = (
        <span
          className="presence-history__emoji"
          role="img"
          aria-label={`Status emoji: ${item.payload.emoji}`}
        >
          {item.payload.emoji}
        </span>
      );
    }
    payloadContent = (
      <p className="presence-history__payload">
        <span className="presence-history__text">{item.payload.text}</span>
        {emoji}
      </p>
    );
  } else {
    payloadContent = (
      <p className="presence-history__unsupported">
        Activity details are unavailable in this version.
      </p>
    );
  }

  let intervalEnd: ReactNode;
  if (endedAt === null) {
    intervalEnd = <span className="presence-history__ongoing">Ongoing</span>;
  } else {
    intervalEnd = (
      <>
        <span>
          Ended <time dateTime={endedAt}>{formatTimestamp(endedAt)}</time>
        </span>
        <span aria-hidden="true">·</span>
        <span>{formatDuration(item.startedAt, endedAt)}</span>
      </>
    );
  }

  return (
    <article
      id={itemElementId(item.id)}
      className="presence-history__item"
      tabIndex={-1}
      aria-labelledby={headingId}
    >
      <div className="presence-history__rail" aria-hidden="true">
        <span className="presence-history__icon" data-category-icon>
          {presentation.icon}
        </span>
        <span className="presence-history__rail-line" />
        <span className="presence-history__rail-end" />
      </div>
      <div className="presence-history__item-content">
        <div className="presence-history__item-header">
          <h4 id={headingId}>{presentation.label}</h4>
          <time dateTime={item.startedAt}>{formatTimestamp(item.startedAt)}</time>
        </div>

        {payloadContent}
        <div className="presence-history__item-end">{intervalEnd}</div>
      </div>
    </article>
  );
}

interface ReadyHistoryFeedProps {
  readonly state: ReadyPresenceHistoryFeedState;
  readonly onLoadMore: () => void;
}

function ReadyHistoryFeed({ state, onLoadMore }: ReadyHistoryFeedProps) {
  let pausedNotice: ReactNode = null;
  if (state.settings.reconsentRequired && state.items.length > 0) {
    pausedNotice = (
      <p className="presence-history__paused">
        Recording is paused until you review the updated terms. Existing unexpired history remains
        available.
      </p>
    );
  }

  let historyContent: ReactNode;
  if (state.items.length === 0) {
    historyContent = <EmptyHistory settings={state.settings} />;
  } else {
    historyContent = (
      <ol className="presence-history__timeline" aria-label="Activity History timeline">
        {state.items.map((item) => (
          <li key={item.id} className="presence-history__timeline-item">
            <HistoryItemCard item={item} />
          </li>
        ))}
      </ol>
    );
  }

  let loadMoreError: ReactNode = null;
  if (state.loadMoreState === 'error') {
    loadMoreError = (
      <div className="presence-history__load-error">
        <p>Could not load more Activity History.</p>
        <button id={LOAD_MORE_RETRY_ID} type="button" onClick={onLoadMore}>
          Retry load more
        </button>
      </div>
    );
  }

  let loadMoreButton: ReactNode = null;
  if (state.nextCursor !== null && state.loadMoreState !== 'error') {
    const label = state.loadMoreState === 'loading' ? 'Loading more…' : 'Load more';
    loadMoreButton = (
      <button
        type="button"
        className="presence-history__load-more"
        onClick={onLoadMore}
        disabled={state.loadMoreState === 'loading'}
      >
        {label}
      </button>
    );
  }

  return (
    <>
      {pausedNotice}
      {historyContent}
      {loadMoreError}
      {loadMoreButton}
    </>
  );
}

export default function PresenceHistorySection({ userId }: PresenceHistorySectionProps) {
  const capability = useClientConfigStore((state) => state.activityHistoryCapability);
  const staleSupported = capability.status === 'error' && capability.lastConfirmedSupported;
  const historyCapabilityUsable =
    capability.status === 'supported' ||
    capability.status === 'confirmed-unsupported' ||
    staleSupported;
  const ownerRef = useRef<string | null>(userId);
  ownerRef.current = userId;
  const generationRef = useRef(0);
  const initialControllerRef = useRef<AbortController | null>(null);
  const loadMoreControllerRef = useRef<AbortController | null>(null);
  const [feedState, setFeedState] = useState<PresenceHistoryFeedState>(() =>
    emptyFeedState(userId)
  );
  const feedStateRef = useRef(feedState);
  feedStateRef.current = feedState;
  const [announcement, setAnnouncement] = useState<OwnerMessage>({
    ownerId: userId,
    message: '',
  });
  const pendingFocusRef = useRef<{ ownerId: string; elementId: string } | null>(null);
  const previousCapabilityRef = useRef({
    status: capability.status,
    usable: historyCapabilityUsable,
  });

  const startInitialLoad = useCallback(
    (reason: InitialLoadReason, preserveConfirmed = false) => {
      if (!userId) return;
      const priorState = feedStateRef.current;
      let priorConfirmed: ReadyPresenceHistoryFeedState | null = null;
      if (priorState.ownerId === userId && priorState.status === 'ready') {
        priorConfirmed = priorState;
      } else if (priorState.ownerId === userId && priorState.status === 'error') {
        priorConfirmed = priorState.confirmed;
      }
      const confirmed = preserveConfirmed ? priorConfirmed : null;
      initialControllerRef.current?.abort();
      loadMoreControllerRef.current?.abort();
      const controller = new AbortController();
      initialControllerRef.current = controller;
      loadMoreControllerRef.current = null;
      const generation = generationRef.current + 1;
      generationRef.current = generation;
      if (confirmed === null) {
        setFeedState({ ownerId: userId, status: 'loading' });
      }
      setAnnouncement({ ownerId: userId, message: '' });
      pendingFocusRef.current = null;

      Promise.all([
        getPresenceHistorySettings(controller.signal),
        getPresenceHistoryPage({ signal: controller.signal }),
      ])
        .then(([settingsResult, page]) => {
          if (!isCurrentRequest(controller, generation, userId, generationRef, ownerRef)) return;
          setFeedState({
            ownerId: userId,
            status: 'ready',
            settings: settingsResult,
            items: page.items,
            nextCursor: page.nextCursor,
            loadMoreState: 'idle',
          });
          setAnnouncement({
            ownerId: userId,
            message: initialAnnouncement(page.items.length, reason),
          });
        })
        .catch((error: unknown) => {
          if (!isCurrentRequest(controller, generation, userId, generationRef, ownerRef)) return;
          const routeMissing = isMissingHistoryRoute(error);
          setFeedState({
            ownerId: userId,
            status: 'error',
            confirmed: routeMissing ? null : confirmed,
            routeMissing,
          });
          setAnnouncement({ ownerId: userId, message: 'Activity History request failed.' });
        })
        .finally(() => {
          if (initialControllerRef.current === controller) {
            initialControllerRef.current = null;
          }
        });
    },
    [userId]
  );

  useEffect(() => {
    initialControllerRef.current?.abort();
    loadMoreControllerRef.current?.abort();
    const resetGeneration = generationRef.current + 1;
    generationRef.current = resetGeneration;
    pendingFocusRef.current = null;

    Promise.resolve().then(() => {
      if (generationRef.current !== resetGeneration || ownerRef.current !== userId) return;
      setFeedState(emptyFeedState(userId));
      setAnnouncement({ ownerId: userId, message: '' });
      if (historyCapabilityUsable && userId) startInitialLoad('initial');
    });

    return () => {
      generationRef.current += 1;
      initialControllerRef.current?.abort();
      loadMoreControllerRef.current?.abort();
    };
  }, [historyCapabilityUsable, startInitialLoad, userId]);

  useEffect(() => {
    const previous = previousCapabilityRef.current;
    previousCapabilityRef.current = {
      status: capability.status,
      usable: historyCapabilityUsable,
    };
    const drainBoundaryCrossed =
      previous.status === 'confirmed-unsupported' || capability.status === 'confirmed-unsupported';
    if (
      previous.usable &&
      historyCapabilityUsable &&
      previous.status !== capability.status &&
      drainBoundaryCrossed &&
      userId
    ) {
      Promise.resolve().then(() => startInitialLoad('initial', true));
    }
  }, [capability.status, historyCapabilityUsable, startInitialLoad, userId]);

  useEffect(() => {
    const pendingFocus = pendingFocusRef.current;
    if (pendingFocus?.ownerId !== userId) return;
    const target = document.getElementById(pendingFocus.elementId);
    if (!target) return;
    target.focus();
    pendingFocusRef.current = null;
  }, [feedState, userId]);

  const loadMore = useCallback(() => {
    if (!userId) return;
    const current = feedState.ownerId === userId ? feedState : emptyFeedState(userId);
    if (
      current.status !== 'ready' ||
      current.nextCursor === null ||
      current.loadMoreState === 'loading'
    ) {
      return;
    }

    loadMoreControllerRef.current?.abort();
    const controller = new AbortController();
    loadMoreControllerRef.current = controller;
    const generation = generationRef.current;
    const cursor = current.nextCursor;
    setFeedState({ ...current, loadMoreState: 'loading' });

    getPresenceHistoryPage({ before: cursor, signal: controller.signal })
      .then((page) => {
        if (!isCurrentRequest(controller, generation, userId, generationRef, ownerRef)) return;
        setFeedState((latest) => {
          if (latest.ownerId !== userId || latest.status !== 'ready') return latest;
          return {
            ...latest,
            items: [...latest.items, ...page.items],
            nextCursor: page.nextCursor,
            loadMoreState: 'idle',
          };
        });
        const firstNewItem = page.items[0];
        if (firstNewItem) {
          pendingFocusRef.current = {
            ownerId: userId,
            elementId: itemElementId(firstNewItem.id),
          };
        } else if (page.nextCursor === null) {
          const lastVisibleItem = current.items.at(-1);
          pendingFocusRef.current = {
            ownerId: userId,
            elementId:
              lastVisibleItem === undefined
                ? 'presence-history-heading'
                : itemElementId(lastVisibleItem.id),
          };
        }
        setAnnouncement({
          ownerId: userId,
          message: loadMoreAnnouncement(page.items.length, page.nextCursor === null),
        });
      })
      .catch(() => {
        if (!isCurrentRequest(controller, generation, userId, generationRef, ownerRef)) return;
        pendingFocusRef.current = { ownerId: userId, elementId: LOAD_MORE_RETRY_ID };
        setFeedState((latest) => {
          if (latest.ownerId !== userId || latest.status !== 'ready') return latest;
          return { ...latest, loadMoreState: 'error' };
        });
        setAnnouncement({
          ownerId: userId,
          message: 'Additional Activity History could not be loaded.',
        });
      })
      .finally(() => {
        if (loadMoreControllerRef.current === controller) {
          loadMoreControllerRef.current = null;
        }
      });
  }, [feedState, userId]);

  const currentState = feedState.ownerId === userId ? feedState : emptyFeedState(userId);
  if (
    capability.status === 'confirmed-unsupported' &&
    (currentState.status === 'idle' ||
      currentState.status === 'loading' ||
      (currentState.status === 'error' &&
        currentState.confirmed === null &&
        currentState.routeMissing))
  ) {
    return null;
  }
  const currentAnnouncement = announcement.ownerId === userId ? announcement.message : '';
  let body: ReactNode;

  if (capability.status === 'loading') {
    body = <p className="presence-history__state">Checking Activity History availability…</p>;
  } else if (capability.status === 'error' && !staleSupported) {
    body = <CapabilityAvailabilityNotice staleSupported={false} />;
  } else if (!userId) {
    body = <p className="presence-history__state">Your profile is still loading.</p>;
  } else if (currentState.status === 'loading' || currentState.status === 'idle') {
    body = <p className="presence-history__state">Loading Activity History…</p>;
  } else if (currentState.status === 'error') {
    const confirmedState = currentState.confirmed;
    body = (
      <>
        <div className="presence-history__state presence-history__state--error" role="alert">
          <p>Activity History could not be loaded.</p>
          <button type="button" onClick={() => startInitialLoad('retry', true)}>
            Retry loading Activity History
          </button>
        </div>
        {confirmedState !== null && (
          <ReadyHistoryFeed
            state={{ ...confirmedState, nextCursor: null, loadMoreState: 'idle' }}
            onLoadMore={loadMore}
          />
        )}
      </>
    );
  } else {
    body = <ReadyHistoryFeed state={currentState} onLoadMore={loadMore} />;
  }

  return (
    <section
      id="section-presence-history"
      className="presence-history"
      aria-labelledby="presence-history-heading"
    >
      <h3
        id="presence-history-heading"
        className="settings-section-title presence-history__heading"
        tabIndex={-1}
      >
        Activity History
      </h3>
      {staleSupported && <CapabilityAvailabilityNotice staleSupported />}
      {capability.status === 'confirmed-unsupported' &&
        (currentState.status === 'ready' ||
          (currentState.status === 'error' && currentState.confirmed !== null)) && (
          <aside
            className="presence-history__state presence-history__state--stale"
            aria-label="Activity History activation paused"
          >
            New opt-ins are paused. Your existing history remains available during this drain.
          </aside>
        )}
      {body}
      <output className="presence-history__announcement" aria-live="polite" aria-atomic="true">
        {currentAnnouncement}
      </output>
    </section>
  );
}
