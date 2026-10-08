import { useRouteError } from 'react-router';
import { ApiError } from '../api/client';
import { Banner, Button } from '../ds';

/**
 * What a route shows when it fails to render or load: what happened and who can fix it, never a stack
 * trace. An ApiError already carries both sentences; anything else gets the generic ones.
 */
export function RouteError() {
  const error = useRouteError();
  const title = error instanceof ApiError ? error.message : 'This page hit a problem.';
  const who = error instanceof ApiError ? error.who : 'Reload it to try again. If it keeps happening, whoever runs this Kivali server can look into it.';
  return (
    <div className="app-route-error">
      <Banner
        tone="danger"
        title={title}
        action={
          <Button size="sm" onClick={() => window.location.reload()}>
            Reload
          </Button>
        }
      >
        {who}
      </Banner>
    </div>
  );
}
