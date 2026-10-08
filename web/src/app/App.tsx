import { useState } from 'react';
import { RouterProvider } from 'react-router/dom';
import { TooltipProvider } from '../ds';
import { createAppRouter } from './router';
import '../styles/frame.css';

export function App() {
  // Created once per mount; StrictMode's second render reuses the first state.
  const [router] = useState(createAppRouter);
  return (
    <TooltipProvider>
      <RouterProvider router={router} />
    </TooltipProvider>
  );
}
