import { Link } from '@tanstack/react-router';

export function NotFoundPage() {
  return (
    <div className="screen">
      <div className="page-header">
        <div className="page-header__text">
          <h1 className="page-header__title">Page not found</h1>
          <p className="page-header__desc">
            This address does not match any page. <Link to="/">Go to the overview</Link>.
          </p>
        </div>
      </div>
    </div>
  );
}
