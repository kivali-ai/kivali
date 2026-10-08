import { createContext, useContext, useLayoutEffect } from 'react';

/** Content column widths from the design: 880 content, 760 transcript and proposals, 640 reading, 1200 the Work board. */
export type ContentWidth = 'content' | 'transcript' | 'reading' | 'wide';

export interface FrameChrome {
  /** The page name: the phone header title and the tab title ("Kivali · <title>"). Defaults to the route's name. */
  title?: string;
  /** False on agent chat, proposal and assignment detail routes. Default true. */
  tabBar?: boolean;
  /** A back link, shown in the phone header and above the content on desktop. */
  back?: { to: string; label: string };
  /** The content column width. Default `content` (880). */
  width?: ContentWidth;
}

export interface FrameChromeControl {
  setChrome(chrome: FrameChrome | null): void;
}

export const FrameChromeContext = createContext<FrameChromeControl | null>(null);

/**
 * A screen's say over the frame: hide the tab bar, show a back link, name the page, widen the column.
 * The setting lasts while the screen is mounted. Pass a stable-looking object; only its values are compared.
 */
export function useFrameChrome(chrome: FrameChrome): void {
  const control = useContext(FrameChromeContext);
  const { title, tabBar, width } = chrome;
  const backTo = chrome.back?.to;
  const backLabel = chrome.back?.label;
  const setChrome = control?.setChrome;
  useLayoutEffect(() => {
    if (!setChrome) return;
    const next: FrameChrome = {};
    if (title !== undefined) next.title = title;
    if (tabBar !== undefined) next.tabBar = tabBar;
    if (width !== undefined) next.width = width;
    if (backTo !== undefined && backLabel !== undefined) next.back = { to: backTo, label: backLabel };
    setChrome(next);
    return () => setChrome(null);
  }, [setChrome, title, tabBar, width, backTo, backLabel]);
}
