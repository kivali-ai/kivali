import * as React from 'react';
/**
 * Props for AgentAvatar.
 */
export interface AgentAvatarProps {
  className?: string;
  name: string;
  role?: string;
  color?: string;
  initials?: string;
  size?: 16 | 20 | 24 | 32 | 40 | 56 }
export declare function AgentAvatar(props: AgentAvatarProps): JSX.Element;
export declare const ROLE_ICONS: Record<string, any[]>;
export declare const roleIconNames: string[];
export declare const identityColors: string[];
export declare function initialsFor(name?: string): string;
export declare function colorFor(name?: string): string;
