import React, { useCallback, useEffect, useState } from 'react';
import ContextMenu from './ContextMenu';
import { copyText, readText, cutSelection, selectAll } from '../../utils/clipboard';

/* ------------------------------------------------------------------ */
/*  Target resolution types                                           */
/* ------------------------------------------------------------------ */

type ContextTarget =
  | { kind: 'link'; href: string; text: string }
  | { kind: 'image'; src: string }
  | { kind: 'text-selection'; text: string }
  | { kind: 'text-input'; element: HTMLInputElement | HTMLTextAreaElement }
  | { kind: 'contenteditable'; element: HTMLElement }
  | { kind: 'area'; area: string }
  | { kind: 'generic' };

interface MenuState {
  target: ContextTarget;
  position: { x: number; y: number };
}

/* ------------------------------------------------------------------ */
/*  Target resolution: walk up the DOM to identify what was clicked   */
/* ------------------------------------------------------------------ */

const TEXT_INPUT_TYPES = new Set(['text', 'search', 'url', 'email', 'password', 'tel', 'number']);

export const DIRECT_MESSAGES_CONTEXT_AREA = 'direct-messages';

/** Try to identify a recognizable context target from a single DOM node. */
function matchNode(node: HTMLElement): ContextTarget | null {
  if (node.tagName === 'A' && (node as HTMLAnchorElement).href) {
    return { kind: 'link', href: (node as HTMLAnchorElement).href, text: node.textContent ?? '' };
  }
  if (node.tagName === 'IMG') {
    return { kind: 'image', src: (node as HTMLImageElement).src };
  }
  if (node.tagName === 'INPUT' && TEXT_INPUT_TYPES.has((node as HTMLInputElement).type)) {
    return { kind: 'text-input', element: node as HTMLInputElement };
  }
  if (node.tagName === 'TEXTAREA') {
    return { kind: 'text-input', element: node as HTMLTextAreaElement };
  }
  if (node.isContentEditable || node.getAttribute('contenteditable') === 'true') {
    return { kind: 'contenteditable', element: node };
  }
  if (node.dataset?.contextArea) {
    return { kind: 'area', area: node.dataset.contextArea };
  }
  return null;
}

/** Check whether the selection's anchor or focus node is within the element. */
function selectionIntersects(el: HTMLElement, selection: Selection): boolean {
  const contains = (node: Node | null): boolean => {
    let n: Node | null = node;
    while (n) {
      if (n === el) return true;
      n = n.parentNode;
    }
    return false;
  };
  return contains(selection.anchorNode) || contains(selection.focusNode);
}

export function resolveTarget(el: Element | null): ContextTarget {
  if (!el || !(el instanceof HTMLElement)) return { kind: 'generic' };

  // Selected text takes priority, but only if the selection intersects the
  // clicked element — avoids showing "Copy" when a stale selection exists
  // elsewhere on the page.
  const selection = globalThis.getSelection();
  if (selection?.toString().trim() && selectionIntersects(el, selection)) {
    return { kind: 'text-selection', text: selection.toString() };
  }

  // Walk up the DOM tree to find a recognizable target
  let current: HTMLElement | null = el;
  while (current) {
    const match = matchNode(current);
    if (match) return match;
    current = current.parentElement;
  }

  return { kind: 'generic' };
}

/* ------------------------------------------------------------------ */
/*  Generic context menu — renders appropriate items per target type  */
/* ------------------------------------------------------------------ */

interface GenericMenuProps {
  target: ContextTarget;
  position: { x: number; y: number };
  onClose: () => void;
}

const GenericMenu: React.FC<GenericMenuProps> = ({ target, position, onClose }) => {
  const handleCopy = async (text: string) => {
    await copyText(text);
    onClose();
  };

  const handlePaste = async (element: HTMLInputElement | HTMLTextAreaElement | HTMLElement) => {
    const result = await readText();
    if (result.success && result.text) {
      if (element instanceof HTMLInputElement || element instanceof HTMLTextAreaElement) {
        // Insert at cursor position
        const start = element.selectionStart ?? element.value.length;
        const end = element.selectionEnd ?? element.value.length;
        const before = element.value.substring(0, start);
        const after = element.value.substring(end);
        const newValue = before + result.text + after;
        // Use native input setter to trigger React's synthetic onChange when
        // available, and fall back to direct assignment otherwise.
        const nativeSetter = Object.getOwnPropertyDescriptor(
          element instanceof HTMLTextAreaElement
            ? HTMLTextAreaElement.prototype
            : HTMLInputElement.prototype,
          'value'
        )?.set;
        if (nativeSetter) {
          nativeSetter.call(element, newValue);
        } else {
          element.value = newValue;
        }
        element.dispatchEvent(new Event('input', { bubbles: true }));
        // Move cursor after pasted text
        const newPos = start + result.text.length;
        element.setSelectionRange(newPos, newPos);
      } else if (element.isContentEditable) {
        // Insert text at the current cursor position in contenteditable
        const sel = globalThis.getSelection();
        if (sel?.rangeCount) {
          const range = sel.getRangeAt(0);
          range.deleteContents();
          range.insertNode(document.createTextNode(result.text));
          range.collapse(false);
        }
      }
    }
    onClose();
  };

  const handleCut = async (element: HTMLInputElement | HTMLTextAreaElement) => {
    await cutSelection(element);
    onClose();
  };

  const handleSelectAll = (element: HTMLElement) => {
    selectAll(element);
    onClose();
  };

  const handleOpenInBrowser = (url: string) => {
    globalThis.open(url, '_blank');
    onClose();
  };

  switch (target.kind) {
    case 'link':
      return (
        <ContextMenu position={position} onClose={onClose}>
          <ContextMenu.Item label="Copy Link" onClick={() => handleCopy(target.href)} />
          <ContextMenu.Item
            label="Open in Browser"
            onClick={() => handleOpenInBrowser(target.href)}
          />
        </ContextMenu>
      );

    case 'image':
      return (
        <ContextMenu position={position} onClose={onClose}>
          <ContextMenu.Item label="Copy Image Link" onClick={() => handleCopy(target.src)} />
          <ContextMenu.Item
            label="Open in Browser"
            onClick={() => handleOpenInBrowser(target.src)}
          />
        </ContextMenu>
      );

    case 'text-selection':
      return (
        <ContextMenu position={position} onClose={onClose}>
          <ContextMenu.Item label="Copy" onClick={() => handleCopy(target.text)} />
        </ContextMenu>
      );

    case 'text-input':
      return (
        <ContextMenu position={position} onClose={onClose}>
          <ContextMenu.Item
            label="Cut"
            onClick={() => handleCut(target.element)}
            disabled={target.element.selectionStart === target.element.selectionEnd}
          />
          <ContextMenu.Item
            label="Copy"
            onClick={() => {
              const start = target.element.selectionStart ?? 0;
              const end = target.element.selectionEnd ?? 0;
              handleCopy(target.element.value.substring(start, end));
            }}
            disabled={target.element.selectionStart === target.element.selectionEnd}
          />
          <ContextMenu.Item label="Paste" onClick={() => handlePaste(target.element)} />
          <ContextMenu.Separator />
          <ContextMenu.Item label="Select All" onClick={() => handleSelectAll(target.element)} />
        </ContextMenu>
      );

    case 'contenteditable':
      return (
        <ContextMenu position={position} onClose={onClose}>
          <ContextMenu.Item
            label="Copy"
            onClick={() => {
              const sel = globalThis.getSelection();
              if (sel?.toString()) handleCopy(sel.toString());
              else onClose();
            }}
            // eslint-disable-next-line @eslint-react/purity -- globalThis.getSelection() is a read-only DOM query to reflect current selection state; no observable side effect
            disabled={!globalThis.getSelection()?.toString()}
          />
          <ContextMenu.Item label="Paste" onClick={() => handlePaste(target.element)} />
          <ContextMenu.Separator />
          <ContextMenu.Item label="Select All" onClick={() => handleSelectAll(target.element)} />
        </ContextMenu>
      );

    case 'area':
      if (target.area === DIRECT_MESSAGES_CONTEXT_AREA) return null;
      return (
        <ContextMenu position={position} onClose={onClose}>
          <ContextMenu.Header>{areaLabel(target.area)}</ContextMenu.Header>
          {areaItems(target.area, onClose)}
        </ContextMenu>
      );

    case 'generic':
      // No meaningful menu for truly generic areas — just suppress default.
      // Returning null means no menu appears, which is better than an empty popup.
      return null;
  }
};

/* ------------------------------------------------------------------ */
/*  Area-specific fallback menus                                      */
/* ------------------------------------------------------------------ */

function areaLabel(area: string): string {
  switch (area) {
    case 'chat':
      return 'Chat';
    case 'members':
      return 'Members';
    case 'channels':
      return 'Channels';
    case 'servers':
      return 'Servers';
    default:
      return '';
  }
}

function areaItems(area: string, onClose: () => void): React.ReactNode {
  switch (area) {
    case 'chat':
      return <ContextMenu.Item label="Mark Channel as Read" disabled onClick={onClose} />;
    case 'members':
      return <ContextMenu.Item label="Copy Server ID" disabled onClick={onClose} />;
    case 'channels':
      return (
        <>
          <ContextMenu.Item label="Create Channel" disabled onClick={onClose} />
          <ContextMenu.Item label="Create Category" disabled onClick={onClose} />
        </>
      );
    default:
      return null;
  }
}

/* ------------------------------------------------------------------ */
/*  Provider — wraps the app, handles global contextmenu events       */
/* ------------------------------------------------------------------ */

interface ContextMenuProviderProps {
  children: React.ReactNode;
}

const ContextMenuProvider: React.FC<ContextMenuProviderProps> = ({ children }) => {
  const [menu, setMenu] = useState<MenuState | null>(null);

  const handleContextMenu = useCallback((e: MouseEvent) => {
    // Always prevent the browser/Electron default context menu
    e.preventDefault();

    const target = resolveTarget(e.target as HTMLElement);
    setMenu({
      target,
      position: { x: e.clientX, y: e.clientY },
    });
  }, []);

  const handleKeyDown = useCallback((e: KeyboardEvent) => {
    // Keyboard accessibility: Shift+F10 or the dedicated ContextMenu key
    if (e.key === 'ContextMenu' || (e.shiftKey && e.key === 'F10')) {
      e.preventDefault();

      const focused = document.activeElement as HTMLElement | null;
      if (!focused) return;

      const rect = focused.getBoundingClientRect();
      const target = resolveTarget(focused);
      setMenu({
        target,
        position: {
          x: rect.left + rect.width / 2,
          y: rect.top + rect.height / 2,
        },
      });
    }
  }, []);

  useEffect(() => {
    document.addEventListener('contextmenu', handleContextMenu);
    document.addEventListener('keydown', handleKeyDown);
    return () => {
      document.removeEventListener('contextmenu', handleContextMenu);
      document.removeEventListener('keydown', handleKeyDown);
    };
  }, [handleContextMenu, handleKeyDown]);

  const closeMenu = useCallback(() => setMenu(null), []);

  return (
    <>
      {children}
      {menu && <GenericMenu target={menu.target} position={menu.position} onClose={closeMenu} />}
    </>
  );
};

export default ContextMenuProvider;
